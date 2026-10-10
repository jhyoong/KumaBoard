// Package app is the agent's behaviour on top of the transport.
package app

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jhyoong/KumaBoard/agent/collectors"
	"github.com/jhyoong/KumaBoard/agent/commands"
	"github.com/jhyoong/KumaBoard/agent/config"
	agentterm "github.com/jhyoong/KumaBoard/agent/terminal"
	"github.com/jhyoong/KumaBoard/agent/transport"
	"github.com/jhyoong/KumaBoard/agent/upgrade"
	"github.com/jhyoong/KumaBoard/internal/buildinfo"
	"github.com/jhyoong/KumaBoard/internal/signing"
	"github.com/jhyoong/KumaBoard/proto"
)

// App implements transport.Handler.
type App struct {
	cfg       *config.Config
	log       *slog.Logger
	runner    *commands.Runner
	upgrader  *upgrade.Upgrader
	startup   upgrade.StartupResult
	collector *collectors.Collector

	mu             sync.Mutex
	cancel         context.CancelFunc
	probationTimer *time.Timer
	send           transport.Sender // current connection, nil while disconnected
	watch          configWatch
	// confirmed is set once a handshake has ended probation, even if the
	// verified report is still to be delivered.
	confirmed bool
	// Why recent connection attempts failed: the last maxConnectFailures of
	// them and how many there were in all. Memory only; a probation rollback
	// reports them.
	connectFailures     []connectFailure
	connectFailureCount int

	// Terminal sessions live under rootCtx, not the control connection, so a
	// reconnect leaves them running. They end when their own socket closes,
	// on Close (process shutdown), or when a reconnect is rejected for auth.
	rootCtx    context.Context
	rootCancel context.CancelFunc
	termMu     sync.Mutex
	terms      map[*liveTerminal]struct{}
}

// connectFailure is one failed connection attempt.
type connectFailure struct {
	at    time.Time
	stage string
	text  string
}

const maxConnectFailures = 5

// liveTerminal is one registered terminal session.
type liveTerminal struct {
	sessionID string
	cancel    context.CancelFunc
	done      chan struct{}
}

// New creates the app.
func New(cfg *config.Config, log *slog.Logger, startupResult upgrade.StartupResult) *App {
	exe, _ := os.Executable()
	workDir := filepath.Dir(exe)
	keys := parseKeys()
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: cfg.CAPool, MinVersion: tls.VersionTLS12}},
	}
	a := &App{
		cfg:      cfg,
		log:      log,
		runner:   commands.New(cfg.Commands, workDir, log),
		startup:  startupResult,
		upgrader: upgrade.New(exe, keys, cfg.Device.Name, cfg.Token, client, log),
		terms:    map[*liveTerminal]struct{}{},
	}
	a.rootCtx, a.rootCancel = context.WithCancel(context.Background())
	a.watch.interval = defaultConfigPoll
	a.watch.poke = make(chan struct{}, 1)
	go a.watchConfig(a.rootCtx)
	base := cfg.Server.URL
	base = strings.TrimSuffix(base, "/ws")
	base = strings.Replace(base, "wss://", "https://", 1)
	base = strings.Replace(base, "ws://", "http://", 1)
	a.upgrader.BaseURL = base
	a.upgrader.ConfigPath = cfg.Path
	// One collector per process, not per connection: GPU backends keep state
	// across ticks. gpu rides in the metrics message, so it needs metrics too.
	a.collector = collectors.New(collectors.Options{
		GPU: a.has(proto.CapGPU) && a.has(proto.CapMetrics),
		Log: log,
	})
	if startupResult.State == upgrade.StateProbation {
		a.probationTimer = time.AfterFunc(upgrade.ProbationTimeout, a.probationExpired)
	}
	return a
}

func parseKeys() []ed25519.PublicKey {
	var keys []ed25519.PublicKey
	for _, h := range []string{buildinfo.ReleaseKeyCurrentHex, buildinfo.ReleaseKeyNextHex} {
		if h == "" {
			continue
		}
		k, err := signing.ParseHexPublicKey(h)
		if err == nil {
			keys = append(keys, k)
		}
	}
	return keys
}

// SetDispatchDelay overrides the delay before an expect_disconnect command
// executes. Useful in tests.
func (a *App) SetDispatchDelay(d time.Duration) { a.runner.DispatchDelay = d }

// SetUpgradeBinaryPath points the upgrader at path instead of the running
// executable: downloads, the swap, the pending marker and the outbox all use
// path and its directory. Call before connecting. Useful in tests, where an
// upgrade that reached the swap would otherwise replace the test binary.
func (a *App) SetUpgradeBinaryPath(path string) {
	a.upgrader.BinaryPath = path
	a.upgrader.BinaryDir = filepath.Dir(path)
}

// SetCollector replaces the metrics collector. Call before connecting.
// Useful in tests.
func (a *App) SetCollector(c *collectors.Collector) { a.collector = c }

// Hello builds the handshake payload. The commands are the current set, so a
// reload that happened while disconnected is declared here.
func (a *App) Hello() proto.Hello {
	cur := a.currentCommands()
	a.mu.Lock()
	// Hello has no config_error field: a pending one follows in a
	// commands_update once connected.
	a.watch.announced = &commandState{defs: cur.defs, problems: cur.problems}
	a.mu.Unlock()
	return proto.Hello{
		ProtocolVersion: proto.Version,
		AgentVersion:    buildinfo.Version,
		DeviceName:      a.cfg.Device.Name,
		Token:           a.cfg.Token,
		OS:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		Capabilities:    a.cfg.Capabilities,
		Commands:        cur.defs,
		Problems:        cur.problems,
	}
}

// OnConnected starts the metrics ticker for this connection.
func (a *App) OnConnected(ack proto.HelloAck, send transport.Sender) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.send = send
	// Covers a config error, or a reload, from between hello and now.
	go a.announceCommands()
	interval := time.Duration(ack.MetricsIntervalS) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if a.has(proto.CapMetrics) {
		go a.metricsLoop(ctx, interval, send)
	}

	sendResult := upgradeSender(send)
	// A failure from before this connection that could not be sent then.
	if sent, err := upgrade.FlushOutbox(a.upgrader.BinaryDir, sendResult); err != nil {
		a.log.Warn("upgrade: kept result not delivered", "err", err)
	} else if sent {
		a.log.Info("upgrade: kept result delivered")
	}

	// Either report is retried on the next connect until Send accepts it.
	switch a.startup.State {
	case upgrade.StateProbation:
		// The handshake itself ends probation, delivered report or not.
		if !a.confirmed {
			a.confirmed = true
			upgrade.ConfirmUpgrade(a.upgrader.BinaryDir, a.upgrader.BinaryPath, a.log)
			if a.probationTimer != nil {
				a.probationTimer.Stop()
			}
		}
		if err := sendResult(a.startupReport(proto.UpgradeVerified, "")); err != nil {
			a.log.Warn("upgrade: verified report not sent", "err", err)
			break
		}
		a.startup.State = upgrade.StateNormal
	case upgrade.StateRolledBack:
		if err := sendResult(a.startupReport(proto.UpgradeRolledBack, a.startup.RollbackReason)); err != nil {
			a.log.Warn("upgrade: rolled_back report not sent", "err", err)
			break
		}
		os.Remove(a.startup.PendingMarker)
		a.startup.State = upgrade.StateNormal
	}
}

// startupReport is the result for the upgrade found by CheckStartup. The
// detail comes from the marker, which the other binary wrote.
func (a *App) startupReport(state, reason string) proto.UpgradeResult {
	return proto.UpgradeResult{
		FromVersion: a.startup.FromVersion,
		ToVersion:   a.startup.ToVersion,
		State:       state,
		Reason:      reason,
		UpgradeID:   a.startup.UpgradeID,
		Detail:      a.upgrader.Detail(a.startup.RollbackDetail),
	}
}

// upgradeSender wraps a connection as an upgrade.Sender.
func upgradeSender(send transport.Sender) upgrade.Sender {
	return func(res proto.UpgradeResult) error {
		e, err := proto.New(proto.TypeUpgradeResult, res)
		if err != nil {
			return err
		}
		return send.Send(e)
	}
}

// OnConnectFailed implements transport.ConnectFailureHandler.
func (a *App) OnConnectFailed(stage string, err error) {
	text := err.Error()
	var he *transport.HandshakeError
	if errors.As(err, &he) {
		// The stage already says "rejected".
		text = fmt.Sprintf("%s: %s", he.Code, he.Message)
		if he.HTTPStatus != 0 {
			text = fmt.Sprintf("HTTP %d", he.HTTPStatus)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.connectFailureCount++
	a.connectFailures = append(a.connectFailures, connectFailure{at: time.Now(), stage: stage, text: text})
	if n := len(a.connectFailures); n > maxConnectFailures {
		a.connectFailures = a.connectFailures[n-maxConnectFailures:]
	}
}

// probationExpired runs when a new binary has not completed a handshake
// within upgrade.ProbationTimeout. It rolls back, which exits unless there is
// no previous binary to restore.
func (a *App) probationExpired() {
	a.mu.Lock()
	defer a.mu.Unlock()
	// A handshake may have won the race with the timer.
	if a.confirmed || a.startup.State != upgrade.StateProbation {
		return
	}
	upgrade.Rollback(a.upgrader.BinaryDir, a.upgrader.BinaryPath, a.startup.PendingMarker,
		proto.UpgradeReasonNoHandshake, a.probationDetail(), a.log)
}

// probationDetail says why the new binary is being rolled back: one summary
// line, then the last connect failures (times in UTC). Callers hold a.mu.
func (a *App) probationDetail() string {
	lines := []string{fmt.Sprintf("no handshake within %s of starting %s (%d connect attempts)",
		upgrade.ProbationTimeout, a.startup.ToVersion, a.connectFailureCount)}
	for _, f := range a.connectFailures {
		lines = append(lines, fmt.Sprintf("%s %s: %s", f.at.UTC().Format("15:04:05"), f.stage, f.text))
	}
	return a.upgrader.Detail(lines...)
}

// OnDisconnected stops per-connection work.
func (a *App) OnDisconnected() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	a.send = nil
}

// OnMessage handles server messages.
func (a *App) OnMessage(env *proto.Envelope, send transport.Sender) {
	switch env.Type {
	case proto.TypeCommandRequest:
		var req proto.CommandRequest
		if err := env.Unmarshal(&req); err != nil {
			return
		}
		// Only the name is read from the request, and the run ID is the
		// envelope ID: the server never supplies arguments.
		go a.runner.Execute(context.Background(), env.ID, req.Name, func(res proto.CommandResult) error {
			reply, err := proto.Reply(env, proto.TypeCommandResult, res)
			if err != nil {
				return err
			}
			return send.Send(reply)
		}, commandOutput(env, send))
	case proto.TypeCommandCancel:
		// No payload: the envelope ID names the run. Unknown IDs are ignored.
		if !a.runner.Cancel(env.ID) {
			a.log.Debug("cancel for unknown run", "id", env.ID)
		}
	case proto.TypeUpgradeRequest:
		var req proto.UpgradeRequest
		if err := env.Unmarshal(&req); err != nil {
			return
		}
		go a.upgrader.HandleUpgradeRequest(req, upgradeSender(send))
	case proto.TypeTerminalOpen:
		var req proto.TerminalOpen
		if err := env.Unmarshal(&req); err != nil {
			return
		}
		// The gate is local config only, never what the server claims: a
		// device without the terminal capability refuses before dialing.
		if reason := a.terminalRefusal(); reason != "" {
			a.replyTerminal(env, send, req.SessionID, reason)
			return
		}
		// Registered before the goroutine starts so a revocation that races
		// the open still cancels it.
		ctx, release := a.trackTerminal(req.SessionID)
		go func() {
			defer release()
			a.runTerminal(ctx, env, req, send)
		}()
	default:
		a.log.Debug("unhandled message", "type", env.Type)
	}
}

// OnRejected implements transport.RejectionHandler. When the server refuses
// a (re)connect because this agent's credentials are no longer valid, every
// live terminal is killed at once. The server also closes them on revoke;
// this is defense in depth.
func (a *App) OnRejected(err error) {
	if !errors.Is(err, transport.ErrAuthRejected) {
		return
	}
	if n := a.killTerminals(); n > 0 {
		a.log.Warn("credentials rejected; killed live terminal sessions", "count", n, "err", err)
	}
}

// Close ends every terminal session (process shutdown) and waits up to
// timeout for them to tear down their shells.
func (a *App) Close(timeout time.Duration) {
	a.rootCancel()
	a.termMu.Lock()
	var dones []chan struct{}
	for t := range a.terms {
		dones = append(dones, t.done)
	}
	a.termMu.Unlock()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for _, d := range dones {
		select {
		case <-d:
		case <-deadline.C:
			return
		}
	}
}

// trackTerminal registers a terminal session and returns its context and a
// release func that must be called when the session ends.
func (a *App) trackTerminal(sessionID string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(a.rootCtx)
	t := &liveTerminal{sessionID: sessionID, cancel: cancel, done: make(chan struct{})}
	a.termMu.Lock()
	a.terms[t] = struct{}{}
	a.termMu.Unlock()
	return ctx, func() {
		cancel()
		a.termMu.Lock()
		delete(a.terms, t)
		a.termMu.Unlock()
		close(t.done)
	}
}

// killTerminals cancels every live terminal session and returns how many
// there were. Entries are removed by each session's release.
func (a *App) killTerminals() int {
	a.termMu.Lock()
	defer a.termMu.Unlock()
	for t := range a.terms {
		t.cancel()
	}
	return len(a.terms)
}

// liveTerminalCount reports registered terminal sessions. Used in tests.
func (a *App) liveTerminalCount() int {
	a.termMu.Lock()
	defer a.termMu.Unlock()
	return len(a.terms)
}

func (a *App) has(cap string) bool {
	for _, c := range a.cfg.Capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

// terminalRefusal returns why this agent refuses terminal_open, or "" if it
// may open one.
func (a *App) terminalRefusal() string {
	if !a.has(proto.CapTerminal) {
		return "terminal_capability_disabled"
	}
	if !agentterm.Supported {
		return agentterm.UnsupportedReason
	}
	return ""
}

// replyTerminal sends terminal_open_result: ok when reason is empty, refused
// otherwise. The wire message has no reason field, so it is logged here.
func (a *App) replyTerminal(env *proto.Envelope, send transport.Sender, sessionID, reason string) {
	res := proto.TerminalOpenResult{SessionID: sessionID, Result: "ok"}
	if reason != "" {
		res.Result = "refused"
		a.log.Warn("terminal_open refused", "session", sessionID, "reason", reason)
	}
	if reply, err := proto.Reply(env, proto.TypeTerminalOpenResult, res); err == nil {
		send.Send(reply)
	}
}

// runTerminal opens the session (shell spawned, terminal socket dialed with
// the pinned CA, hello written) and only then replies ok. Any failure before
// that point is a refusal.
func (a *App) runTerminal(ctx context.Context, env *proto.Envelope, req proto.TerminalOpen, send transport.Sender) {
	termURL := strings.TrimSuffix(a.cfg.Server.URL, "/ws") + "/ws/terminal"

	sess := &agentterm.Session{
		URL:       termURL,
		TLS:       &tls.Config{RootCAs: a.cfg.CAPool, MinVersion: tls.VersionTLS12},
		SessionID: req.SessionID,
		Ticket:    req.AgentTicket,
		Cols:      req.Cols,
		Rows:      req.Rows,
		Log:       a.log,
	}
	if err := sess.Open(ctx); err != nil {
		a.log.Error("terminal session failed to open", "session", req.SessionID, "err", err)
		a.replyTerminal(env, send, req.SessionID, "open_failed")
		return
	}
	a.replyTerminal(env, send, req.SessionID, "")
	sess.Serve(ctx)
}

func (a *App) metricsLoop(ctx context.Context, interval time.Duration, send transport.Sender) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		m, err := a.collector.Collect(cctx)
		cancel()
		if err != nil {
			a.log.Warn("metrics collection failed", "err", err)
		} else if env, err := proto.New(proto.TypeMetrics, m); err == nil {
			if err := send.Send(env); err != nil {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
