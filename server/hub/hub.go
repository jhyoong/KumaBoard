// Package hub owns agent control-socket sessions.
package hub

import (
	"context"
	"crypto/subtle"
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

	// now is the clock for upgrade dispatch records. Tests override it;
	// default is time.Now.
	now func() time.Time
}

// Hub accepts agent connections at /ws and tracks one session per device.
type Hub struct {
	opts     Options
	router   *router
	mu       sync.Mutex
	sessions map[string]*Session // by device name

	// upgradeMu makes "nothing is in flight" and the insert of a new upgrade
	// row one step, so concurrent evaluations cannot both take the fleet slot.
	upgradeMu sync.Mutex
}

// New creates a Hub.
func New(opts Options) *Hub {
	if opts.PongTimeout == 0 {
		opts.PongTimeout = 10 * time.Second
	}
	if opts.afterFunc == nil {
		opts.afterFunc = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	}
	if opts.now == nil {
		opts.now = time.Now
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
		h.noteHandshakeRejected(ctx, d, hello)
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
