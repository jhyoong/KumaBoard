// Package hub owns agent control-socket sessions.
package hub

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/oklog/ulid/v2"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

// Events receives notifications about sessions. The registry implements it.
type Events interface {
	DeviceConnected(name string)
	DeviceDisconnected(name string)
	MetricsReceived(name string, m proto.Metrics)
	RunChanged(runID string)
	// RunOutput is one chunk of a running command's output, already bounded
	// by the server. seq counts from 1 per run and is assigned by the server.
	RunOutput(runID, device string, seq int, o proto.CommandOutput)
	UpgradeChanged(name string)
	// CommandsChanged fires when a device replaced its declared commands
	// while connected.
	CommandsChanged(name string)
}

// Options configures a Hub.
type Options struct {
	Store             *store.Store
	Events            Events
	Log               *slog.Logger
	MetricsInterval   time.Duration
	HeartbeatInterval time.Duration
	PongTimeout       time.Duration // default 10s

	// TerminalRefused, if set, is called when an agent answers terminal_open
	// with anything other than "ok", so the terminal broker can fail the
	// pending session.
	TerminalRefused func(deviceName, sessionID string)

	// afterFunc schedules pong-deadline checks. Tests override it to fire
	// deadlines deterministically; default wraps time.AfterFunc.
	afterFunc func(time.Duration, func())
}

// Hub accepts agent connections at /ws and tracks one session per device.
type Hub struct {
	opts     Options
	router   *router
	mu       sync.Mutex
	sessions map[string]*Session // by device name
}

// New creates a Hub.
func New(opts Options) *Hub {
	if opts.PongTimeout == 0 {
		opts.PongTimeout = 10 * time.Second
	}
	if opts.afterFunc == nil {
		opts.afterFunc = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	}
	return &Hub{opts: opts, router: newRouter(), sessions: map[string]*Session{}}
}

// ServeHTTP upgrades the request and runs the session until it ends.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		h.opts.Log.Warn("ws accept failed", "remote", r.RemoteAddr, "err", err)
		return
	}
	conn.SetReadLimit(proto.MaxMessageSize)
	h.serve(r.Context(), conn, r.RemoteAddr)
}

func (h *Hub) serve(ctx context.Context, conn *websocket.Conn, remote string) {
	defer conn.CloseNow()
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	env, err := readEnvelope(hctx, conn)
	cancel()
	if err != nil || env.Type != proto.TypeHello {
		conn.Close(websocket.StatusProtocolError, "expected hello")
		return
	}
	var hello proto.Hello
	if err := env.Unmarshal(&hello); err != nil {
		conn.Close(websocket.StatusProtocolError, "bad hello")
		return
	}
	sess, code := h.authenticate(ctx, &hello, remote)
	if code != "" {
		e, _ := proto.Reply(env, proto.TypeError, proto.Error{Code: code, Message: "handshake rejected"})
		writeEnvelope(ctx, conn, e)
		conn.Close(websocket.StatusPolicyViolation, code)
		return
	}
	sess.conn = conn
	h.register(ctx, sess)
	defer h.unregister(ctx, sess)

	ack := proto.HelloAck{
		SessionID:          sess.ID,
		ServerTime:         time.Now().UTC(),
		MetricsIntervalS:   int(h.opts.MetricsInterval / time.Second),
		HeartbeatIntervalS: int(h.opts.HeartbeatInterval / time.Second),
	}
	ackEnv, _ := proto.Reply(env, proto.TypeHelloAck, ack)
	if err := sess.Send(ackEnv); err != nil {
		return
	}
	h.opts.Events.DeviceConnected(sess.DeviceName)
	h.opts.Log.Info("agent connected", "device", sess.DeviceName, "version", hello.AgentVersion, "remote", remote)
	go h.checkUpgradeAfterHandshake(ctx, sess, hello.AgentVersion)

	go sess.pinger(ctx, h.opts.HeartbeatInterval, h.opts.PongTimeout, h.opts.afterFunc)
	sess.readLoop(ctx, h)
}

// authenticate checks the device, token, then protocol version, in that order,
// so only an authenticated agent can record a reject reason on its own row.
func (h *Hub) authenticate(ctx context.Context, hello *proto.Hello, remote string) (*Session, string) {
	st := h.opts.Store
	d, err := st.GetDevice(ctx, hello.DeviceName)
	if err != nil {
		st.Audit(ctx, "agent:"+hello.DeviceName, "handshake", hello.DeviceName, proto.ErrUnknownDevice, remote)
		return nil, proto.ErrUnknownDevice
	}
	if d.TokenHash == nil || subtle.ConstantTimeCompare(store.HashToken(hello.Token), d.TokenHash) != 1 {
		st.RecordReject(ctx, d.Name, proto.ErrAuthFailed)
		st.Audit(ctx, "agent:"+d.Name, "handshake", d.Name, proto.ErrAuthFailed, remote)
		return nil, proto.ErrAuthFailed
	}
	if !proto.Supported(hello.ProtocolVersion) {
		st.RecordReject(ctx, d.Name, proto.ErrProtocolVersionUnsupported)
		st.Audit(ctx, "agent:"+d.Name, "handshake", d.Name, proto.ErrProtocolVersionUnsupported,
			"agent protocol "+strconv.Itoa(hello.ProtocolVersion)+" version "+hello.AgentVersion)
		return nil, proto.ErrProtocolVersionUnsupported
	}
	// Everything in hello is untrusted, the command declarations included.
	cmds, problems := proto.SanitizeCommands(hello.Commands), proto.SanitizeProblems(hello.Problems)
	if err := st.RecordHandshake(ctx, d.ID, hello.OS, hello.Arch, hello.AgentVersion, hello.ProtocolVersion, hello.Capabilities, cmds, problems); err != nil {
		h.opts.Log.Error("record handshake", "device", d.Name, "err", err)
		return nil, proto.ErrProtocol
	}
	st.Audit(ctx, "agent:"+d.Name, "handshake", d.Name, "ok", "version "+hello.AgentVersion)
	return &Session{
		ID:              ulid.Make().String(),
		DeviceID:        d.ID,
		DeviceName:      d.Name,
		ProtocolVersion: hello.ProtocolVersion,
		closed:          make(chan struct{}),
		log:             h.opts.Log,
	}, ""
}

func (h *Hub) register(ctx context.Context, s *Session) {
	h.mu.Lock()
	old := h.sessions[s.DeviceName]
	h.sessions[s.DeviceName] = s
	h.mu.Unlock()
	if old != nil {
		h.opts.Log.Info("replacing duplicate session", "device", s.DeviceName)
		h.opts.Store.Audit(ctx, "agent:"+s.DeviceName, "session_replaced", s.DeviceName, "ok", "")
		old.close(websocket.StatusPolicyViolation, proto.ErrDuplicateSession)
	}
}

func (h *Hub) unregister(ctx context.Context, s *Session) {
	s.close(websocket.StatusNormalClosure, "bye")
	h.mu.Lock()
	current := h.sessions[s.DeviceName] == s
	if current {
		delete(h.sessions, s.DeviceName)
	}
	h.mu.Unlock()
	h.sessionEnded(ctx, s)
	if current {
		h.opts.Store.RecordDisconnect(ctx, s.DeviceID)
		h.opts.Events.DeviceDisconnected(s.DeviceName)
		h.opts.Log.Info("agent disconnected", "device", s.DeviceName)
	}
}

// handleMessage dispatches one message after the handshake.
func (h *Hub) handleMessage(ctx context.Context, s *Session, env *proto.Envelope) {
	switch env.Type {
	case proto.TypePong:
		s.pongs.Add(1)
	case proto.TypePing:
		pong, _ := proto.Reply(env, proto.TypePong, nil)
		s.Send(pong)
	case proto.TypeMetrics:
		var m proto.Metrics
		if err := env.Unmarshal(&m); err != nil {
			return
		}
		m.Sanitize()
		h.opts.Store.TouchLastSeen(ctx, s.DeviceID)
		if err := h.opts.Store.RecordMetrics(ctx, s.DeviceID, time.Now(), m); err != nil {
			h.opts.Log.Warn("record metrics", "device", s.DeviceName, "err", err)
		}
		h.opts.Events.MetricsReceived(s.DeviceName, m)
	case proto.TypeCommandResult:
		h.handleCommandResult(ctx, s, env)
	case proto.TypeCommandOutput:
		h.handleCommandOutput(s, env)
	case proto.TypeCommandsUpdate:
		h.handleCommandsUpdate(ctx, s, env)
	case proto.TypeUpgradeResult:
		h.handleUpgradeResult(ctx, s, env)
	case proto.TypeTerminalOpenResult:
		var res proto.TerminalOpenResult
		if err := env.Unmarshal(&res); err != nil {
			return
		}
		if res.Result != "ok" {
			h.opts.Log.Info("terminal refused by agent", "device", s.DeviceName, "session", res.SessionID)
			if h.opts.TerminalRefused != nil {
				h.opts.TerminalRefused(s.DeviceName, res.SessionID)
			}
		}
	default:
		h.opts.Log.Debug("unhandled message", "device", s.DeviceName, "type", env.Type)
	}
}

// Connected reports whether a device has a live session.
func (h *Hub) Connected(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.sessions[name]
	return ok
}

// SessionCount is the number of live sessions.
func (h *Hub) SessionCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions)
}

// ConnectedNames lists devices with live sessions.
func (h *Hub) ConnectedNames() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.sessions))
	for n := range h.sessions {
		out = append(out, n)
	}
	return out
}

// CloseDevice ends a device's session, for example after token revocation.
func (h *Hub) CloseDevice(name, reason string) {
	h.mu.Lock()
	s := h.sessions[name]
	h.mu.Unlock()
	if s != nil {
		s.close(websocket.StatusPolicyViolation, reason)
	}
}

// Session returns the live session for the named device, or nil.
func (h *Hub) Session(name string) *Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[name]
}

func (h *Hub) handleUpgradeResult(ctx context.Context, s *Session, env *proto.Envelope) {
	var res proto.UpgradeResult
	if err := env.Unmarshal(&res); err != nil {
		return
	}
	u, err := h.opts.Store.GetLatestUpgrade(ctx, s.DeviceID)
	if err != nil || u == nil {
		h.opts.Log.Warn("upgrade result for unknown upgrade", "device", s.DeviceName)
		return
	}
	h.opts.Store.UpdateUpgradeState(ctx, u.ID, res.State, res.Reason)
	h.opts.Store.Audit(ctx, "agent:"+s.DeviceName, "upgrade_state", s.DeviceName,
		res.State, res.FromVersion+" -> "+res.ToVersion)
	h.opts.Events.UpgradeChanged(s.DeviceName)
}

func (h *Hub) checkUpgradeAfterHandshake(ctx context.Context, s *Session, agentVersion string) {
	d, err := h.opts.Store.GetDeviceByID(ctx, s.DeviceID)
	if err != nil {
		return
	}
	u, _ := h.opts.Store.GetLatestUpgrade(ctx, d.ID)
	if u != nil && u.IsInFlight() && agentVersion == u.FromVersion {
		h.opts.Store.UpdateUpgradeState(ctx, u.ID, proto.UpgradeRolledBack, "handshake_at_from_version")
		h.opts.Store.Audit(ctx, "server", "upgrade_rollback_detected", d.Name, proto.UpgradeRolledBack, u.ID)
		return
	}
	h.evaluateUpgrade(ctx, d, s, false)
}

// Reasons evaluateUpgrade may decline to send an upgrade request.
var (
	ErrUpgradeNotNeeded    = errors.New("no desired version set or device already at it")
	ErrUpgradeInFlight     = errors.New("another upgrade is already in flight")
	ErrUpgradePrevFailed   = errors.New("a previous upgrade to this version failed")
	ErrUpgradeNoRelease    = errors.New("no release ingested for target version/os/arch")
	ErrUpgradeNotConnected = errors.New("device is not connected")
)

// evaluateUpgrade sends d an upgrade request for its desired agent version and
// returns nil only if the request was sent. When explicit is false (automatic
// offers on handshake or on a desired-version change), a prior failed or
// rolled_back attempt at the same target version blocks the offer so a bad
// release is not retried in a loop. An explicit operator retry skips only
// that check; the one-active-upgrade-in-fleet guard always applies.
func (h *Hub) evaluateUpgrade(ctx context.Context, d *store.Device, s *Session, explicit bool) error {
	if d.DesiredAgentVersion == "" || d.DesiredAgentVersion == d.AgentVersion {
		return ErrUpgradeNotNeeded
	}
	active, err := h.opts.Store.GetActiveUpgrade(ctx)
	if err != nil {
		return fmt.Errorf("check active upgrade: %w", err)
	}
	if active != nil {
		return ErrUpgradeInFlight
	}
	if !explicit && h.opts.Store.HasFailedUpgrade(ctx, d.ID, d.DesiredAgentVersion) {
		return ErrUpgradePrevFailed
	}
	rel, err := h.opts.Store.GetRelease(ctx, d.DesiredAgentVersion, d.OS, d.Arch)
	if err != nil {
		h.opts.Log.Warn("no release for target version", "device", d.Name,
			"version", d.DesiredAgentVersion, "os", d.OS, "arch", d.Arch)
		if errors.Is(err, store.ErrNotFound) {
			return ErrUpgradeNoRelease
		}
		return fmt.Errorf("get release: %w", err)
	}
	requestedBy := "server"
	if explicit {
		requestedBy = "admin"
	}
	id, err := h.opts.Store.CreateUpgrade(ctx, d.ID, d.AgentVersion, d.DesiredAgentVersion, requestedBy)
	if err != nil {
		h.opts.Log.Error("create upgrade row", "err", err)
		return fmt.Errorf("create upgrade row: %w", err)
	}
	url := fmt.Sprintf("/api/agent/releases/%s/%s_%s", rel.Version, rel.OS, rel.Arch)
	req := proto.UpgradeRequest{
		Version: rel.Version, OS: rel.OS, Arch: rel.Arch,
		SHA256: rel.SHA256, SizeBytes: rel.SizeBytes, Signature: rel.Signature,
		URL: url,
	}
	env, _ := proto.New(proto.TypeUpgradeRequest, req)
	if err := s.Send(env); err != nil {
		h.opts.Log.Error("send upgrade request", "device", d.Name, "err", err)
		// Close the row so it does not hold the fleet-wide in-flight slot.
		h.opts.Store.UpdateUpgradeState(ctx, id, proto.UpgradeFailed, "send_failed")
		return fmt.Errorf("send upgrade request: %w", err)
	}
	h.opts.Store.Audit(ctx, "server", "upgrade_request", d.Name, "sent", id)
	h.opts.Log.Info("upgrade request sent", "device", d.Name, "to", d.DesiredAgentVersion)
	return nil
}

// SendUpgradeToDevice triggers an automatic upgrade evaluation for a connected
// device. It returns nil only if an upgrade request was sent.
func (h *Hub) SendUpgradeToDevice(ctx context.Context, d *store.Device) error {
	return h.sendUpgrade(ctx, d, false)
}

// RetryUpgrade is SendUpgradeToDevice for an explicit operator retry: it is
// not blocked by an earlier failed or rolled_back attempt at the same version.
func (h *Hub) RetryUpgrade(ctx context.Context, d *store.Device) error {
	return h.sendUpgrade(ctx, d, true)
}

func (h *Hub) sendUpgrade(ctx context.Context, d *store.Device, explicit bool) error {
	s := h.Session(d.Name)
	if s == nil {
		return ErrUpgradeNotConnected
	}
	return h.evaluateUpgrade(ctx, d, s, explicit)
}
