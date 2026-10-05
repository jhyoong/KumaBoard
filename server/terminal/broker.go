package terminal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/oklog/ulid/v2"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

var (
	ErrSessionLimit = errors.New("terminal: session limit reached")
)

const (
	maxSessionsPerDevice = 2
	defaultTicketTTL     = 30 * time.Second
	defaultIdleTimeout   = 30 * time.Minute
	defaultReapInterval  = 10 * time.Second
	maxIdleCheck         = 30 * time.Second

	// defaultHelloTimeout bounds how long an unauthenticated /ws/terminal
	// socket may stay open before sending its hello (ticket) frame.
	defaultHelloTimeout = 10 * time.Second
	// maxPendingHellos caps concurrent sockets that have not yet sent their
	// hello, across all clients. Further upgrades get 503 until one frees.
	maxPendingHellos = 16
)

// Close reasons sent to the browser (and agent) when a session fails before
// or during pairing.
const (
	ReasonPairingTimeout = "pairing timeout"
	ReasonTicketExpired  = "ticket expired"
	ReasonAgentRefused   = "agent refused"
	ReasonCancelled      = "cancelled"
	ReasonIdleTimeout    = "idle timeout"
	ReasonSessionEnded   = "session ended"
	ReasonHelloTimeout   = "hello timeout"

	// Reasons for sessions ended by an administrative action. Live sessions
	// closed this way are recorded as closed (terminal_close ok) with the
	// reason; pending tickets are voided and recorded as failed opens.
	ReasonTerminalDisabled = "terminal disabled"
	ReasonDeviceRevoked    = "device revoked"
	ReasonLoggedOut        = "logged out"
)

// Options tunes broker timings. Zero values select the defaults.
type Options struct {
	// TicketTTL is the pairing window, measured from ticket issue: both
	// tickets must be presented, and both sides attached, within TicketTTL of
	// CreateTicket. A side that attaches late only waits out the rest of the
	// window (pairing timeout), so pairing never takes longer than TicketTTL.
	TicketTTL time.Duration
	// IdleTimeout ends a paired session after no traffic in either direction.
	IdleTimeout time.Duration
	// ReapInterval is how often expired, never-connected tickets are reaped.
	ReapInterval time.Duration
	// HelloTimeout is how long a new socket has to send its hello frame.
	HelloTimeout time.Duration
}

// Ticket holds the identifiers and metadata for a terminal session handshake.
// The BrowserTicket is consumed by the browser WebSocket and the AgentTicket
// is consumed by the device agent WebSocket.
type Ticket struct {
	SessionID     string
	BrowserTicket string
	AgentTicket   string
	DeviceName    string
	DeviceID      int64
	User          string
	Cols          int
	Rows          int
	CreatedAt     time.Time
}

type session struct {
	ticket   *Ticket
	pairOnce sync.Once
	ready    chan struct{} // closed once both sides are attached
	closed   chan struct{}

	// Guarded by Broker.mu.
	browserUsed bool
	agentUsed   bool

	// Guarded by mu.
	mu         sync.Mutex
	browser    *websocket.Conn
	agent      *websocket.Conn
	paired     bool
	ended      bool
	endReason  string
	lastActive time.Time
	bytesIn    int64
	bytesOut   int64
}

// Broker manages terminal session tickets and session state. It is the single
// source of truth for how many terminal sessions (pending or live) a device has.
type Broker struct {
	store *store.Store
	log   *slog.Logger
	opts  Options

	mu        sync.Mutex
	sessions  map[string]*session   // keyed by session ID
	byBrowser map[[32]byte]*session // keyed by sha256(browser ticket)

	// preHello is a semaphore of sockets that have not sent their hello yet.
	preHello chan struct{}

	stop     chan struct{}
	stopOnce sync.Once
}

// NewBroker returns a Broker with default timings. Both st and log may be nil
// (log defaults to slog.Default).
func NewBroker(st *store.Store, log *slog.Logger) *Broker {
	return NewBrokerWithOptions(st, log, Options{})
}

// NewBrokerWithOptions returns a Broker with the given timings. It closes any
// terminal_sessions rows left open by a previous process (no session survives
// a restart) and starts the background reaper; call Close to stop it.
func NewBrokerWithOptions(st *store.Store, log *slog.Logger, opts Options) *Broker {
	if log == nil {
		log = slog.Default()
	}
	if opts.TicketTTL <= 0 {
		opts.TicketTTL = defaultTicketTTL
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = defaultIdleTimeout
	}
	if opts.ReapInterval <= 0 {
		opts.ReapInterval = defaultReapInterval
	}
	if opts.HelloTimeout <= 0 {
		opts.HelloTimeout = defaultHelloTimeout
	}
	b := &Broker{
		store:     st,
		log:       log,
		opts:      opts,
		sessions:  map[string]*session{},
		byBrowser: map[[32]byte]*session{},
		preHello:  make(chan struct{}, maxPendingHellos),
		stop:      make(chan struct{}),
	}
	if st != nil {
		n, err := st.EndOpenTerminalSessions(context.Background())
		if err != nil {
			log.Error("sweep orphan terminal sessions", "err", err)
		} else if n > 0 {
			log.Info("closed orphan terminal sessions", "count", n)
		}
	}
	go b.reapLoop()
	return b
}

// Close stops the background reaper. Live sessions are not affected.
func (b *Broker) Close() {
	b.stopOnce.Do(func() { close(b.stop) })
}

func (b *Broker) reapLoop() {
	t := time.NewTicker(b.opts.ReapInterval)
	defer t.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-t.C:
			b.reap()
		}
	}
}

// reap fails unpaired sessions past their pairing deadline. It mainly frees
// tickets that no side ever presented; a session with a side attached is
// normally failed by its pair goroutine first. fail leaves paired sessions
// alone, so live sessions are never touched.
func (b *Broker) reap() {
	b.mu.Lock()
	var expired []*session
	for _, s := range b.sessions {
		if b.expired(s) {
			expired = append(expired, s)
		}
	}
	b.mu.Unlock()
	for _, s := range expired {
		b.fail(s, websocket.StatusGoingAway, ReasonTicketExpired)
	}
}

func (b *Broker) expired(s *session) bool {
	return time.Now().After(b.pairDeadline(s))
}

// pairDeadline is the absolute time by which s must be paired.
func (b *Broker) pairDeadline(s *session) time.Time {
	return s.ticket.CreatedAt.Add(b.opts.TicketTTL)
}

// CreateTicket creates a new terminal session ticket for the given device.
// It returns ErrSessionLimit if the device already has maxSessionsPerDevice
// pending or live sessions.
func (b *Broker) CreateTicket(deviceName string, deviceID int64, user string, cols, rows int) (*Ticket, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.activeCountLocked(deviceName) >= maxSessionsPerDevice {
		return nil, ErrSessionLimit
	}

	now := time.Now()
	tk := &Ticket{
		SessionID:     ulid.Make().String(),
		BrowserTicket: randomToken(),
		AgentTicket:   randomToken(),
		DeviceName:    deviceName,
		DeviceID:      deviceID,
		User:          user,
		Cols:          cols,
		Rows:          rows,
		CreatedAt:     now,
	}
	s := &session{
		ticket:     tk,
		ready:      make(chan struct{}),
		closed:     make(chan struct{}),
		lastActive: now,
	}
	b.sessions[tk.SessionID] = s
	b.byBrowser[sha256.Sum256([]byte(tk.BrowserTicket))] = s
	return tk, nil
}

// ConsumeBrowserTicket looks up and consumes a browser ticket. It returns the
// associated Ticket and true on success. Each browser ticket can only be
// consumed once. An expired ticket is rejected and fails its session.
func (b *Broker) ConsumeBrowserTicket(ticket string) (*Ticket, bool) {
	s, ok := b.consumeBrowser(ticket)
	if !ok {
		return nil, false
	}
	return s.ticket, true
}

func (b *Broker) consumeBrowser(ticket string) (*session, bool) {
	b.mu.Lock()
	s := b.byBrowser[sha256.Sum256([]byte(ticket))]
	if s == nil || s.browserUsed ||
		subtle.ConstantTimeCompare([]byte(s.ticket.BrowserTicket), []byte(ticket)) != 1 {
		b.mu.Unlock()
		return nil, false
	}
	if b.expired(s) {
		b.mu.Unlock()
		b.fail(s, websocket.StatusGoingAway, ReasonTicketExpired)
		return nil, false
	}
	s.browserUsed = true // single use
	b.mu.Unlock()
	return s, true
}

// ConsumeAgentTicket looks up and consumes an agent ticket by session ID.
// It returns the associated Ticket and true on success. Each agent ticket
// can only be consumed once. An expired ticket is rejected and fails its
// session (closing a waiting browser).
func (b *Broker) ConsumeAgentTicket(sessionID, ticket string) (*Ticket, bool) {
	s, ok := b.consumeAgent(sessionID, ticket)
	if !ok {
		return nil, false
	}
	return s.ticket, true
}

func (b *Broker) consumeAgent(sessionID, ticket string) (*session, bool) {
	b.mu.Lock()
	s := b.sessions[sessionID]
	if s == nil || s.agentUsed ||
		subtle.ConstantTimeCompare([]byte(s.ticket.AgentTicket), []byte(ticket)) != 1 {
		b.mu.Unlock()
		return nil, false
	}
	if b.expired(s) {
		b.mu.Unlock()
		b.fail(s, websocket.StatusGoingAway, ReasonTicketExpired)
		return nil, false
	}
	s.agentUsed = true // single use
	b.mu.Unlock()
	return s, true
}

// CloseDevice ends every session of deviceName: pending tickets are voided
// (recorded as failed opens) and live sessions are closed (recorded as closed)
// with reason. It returns how many sessions it ended.
func (b *Broker) CloseDevice(deviceName, reason string) int {
	return b.closeMatching(func(t *Ticket) bool { return t.DeviceName == deviceName }, reason)
}

// CloseUser ends every session and ticket created by the dashboard user, as
// CloseDevice does for a device. It returns how many sessions it ended.
func (b *Broker) CloseUser(user, reason string) int {
	return b.closeMatching(func(t *Ticket) bool { return t.User == user }, reason)
}

// closeMatching snapshots the matching sessions under the broker lock, then
// ends each outside it (ending takes the lock to remove the session).
func (b *Broker) closeMatching(match func(*Ticket) bool, reason string) int {
	b.mu.Lock()
	var victims []*session
	for _, s := range b.sessions {
		if match(s.ticket) {
			victims = append(victims, s)
		}
	}
	b.mu.Unlock()
	n := 0
	for _, s := range victims {
		did, wasPaired := b.end(s, false, websocket.StatusPolicyViolation, reason)
		if !did {
			continue
		}
		n++
		if !wasPaired {
			b.recordFailedOpen(s, reason)
		}
		// A paired session's close is recorded by run once its relays stop.
	}
	return n
}

// CancelTicket fails a session that was never paired (e.g. agent send failed).
func (b *Broker) CancelTicket(sessionID string) {
	if s := b.lookup(sessionID); s != nil {
		b.fail(s, websocket.StatusGoingAway, ReasonCancelled)
	}
}

// RefuseSession fails a pending session after the device's agent replied
// refused to terminal_open: both tickets are voided, a waiting browser is
// closed with ReasonAgentRefused, and the device slot is released. It is a
// no-op if the session is unknown, belongs to another device, or is already
// paired; the paired check and the failure are one atomic transition (see
// end), so a session that pairs concurrently is either refused or opened,
// never both. It reports whether a session was failed.
func (b *Broker) RefuseSession(deviceName, sessionID string) bool {
	s := b.lookup(sessionID)
	if s == nil || s.ticket.DeviceName != deviceName {
		return false
	}
	return b.fail(s, websocket.StatusPolicyViolation, ReasonAgentRefused)
}

func (b *Broker) lookup(sessionID string) *session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions[sessionID]
}

func (b *Broker) removeSession(s *session) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sessions, s.ticket.SessionID)
	delete(b.byBrowser, sha256.Sum256([]byte(s.ticket.BrowserTicket)))
}

// ActiveCount returns the number of pending or live sessions for the given
// device.
func (b *Broker) ActiveCount(deviceName string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.activeCountLocked(deviceName)
}

func (b *Broker) activeCountLocked(deviceName string) int {
	n := 0
	for _, s := range b.sessions {
		if s.ticket.DeviceName == deviceName {
			n++
		}
	}
	return n
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("terminal: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// ServeHTTP upgrades the HTTP request to a WebSocket connection, reads a JSON
// hello message to determine whether the caller is a browser or agent, then
// delegates to the appropriate handler.
//
// Until its hello arrives a socket is unauthenticated, so at most
// maxPendingHellos such sockets may exist at once (extra upgrade requests get
// 503 Service Unavailable) and each must send its hello within HelloTimeout
// or be closed with ReasonHelloTimeout.
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case b.preHello <- struct{}{}:
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many pending terminal connections", http.StatusServiceUnavailable)
		return
	}
	released := false
	release := func() {
		if !released {
			released = true
			<-b.preHello
		}
	}
	defer release()

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(proto.TerminalMaxFrame)

	// Close with a reason rather than cancelling the Read, which would drop
	// the connection without a close frame.
	timer := time.AfterFunc(b.opts.HelloTimeout, func() {
		conn.Close(websocket.StatusPolicyViolation, ReasonHelloTimeout)
	})
	typ, data, err := conn.Read(r.Context())
	if !timer.Stop() {
		return // timed out; the close is already under way
	}
	release()
	if err != nil || typ != websocket.MessageText {
		conn.Close(websocket.StatusProtocolError, "expected text hello")
		return
	}

	var hello struct {
		Ticket      string `json:"ticket"`
		SessionID   string `json:"session_id"`
		AgentTicket string `json:"agent_ticket"`
	}
	if err := json.Unmarshal(data, &hello); err != nil {
		conn.Close(websocket.StatusProtocolError, "bad hello")
		return
	}

	if hello.Ticket != "" {
		b.handleBrowser(conn, hello.Ticket)
	} else if hello.SessionID != "" {
		b.handleAgent(conn, hello.SessionID, hello.AgentTicket)
	} else {
		conn.Close(websocket.StatusProtocolError, "missing ticket or session_id")
	}
}

func (b *Broker) handleBrowser(conn *websocket.Conn, ticket string) {
	s, ok := b.consumeBrowser(ticket)
	if !ok {
		conn.Close(websocket.StatusPolicyViolation, "invalid or expired ticket")
		return
	}
	if !b.attach(s, conn, true) {
		conn.Close(websocket.StatusPolicyViolation, "session closed")
		return
	}
	<-s.closed
}

func (b *Broker) handleAgent(conn *websocket.Conn, sessionID, ticket string) {
	s, ok := b.consumeAgent(sessionID, ticket)
	if !ok {
		conn.Close(websocket.StatusPolicyViolation, "invalid or expired agent ticket")
		return
	}
	if !b.attach(s, conn, false) {
		conn.Close(websocket.StatusPolicyViolation, "session closed")
		return
	}
	<-s.closed
}

// attach registers one side's socket. The first side to attach starts the
// pairing wait; the second releases the relay.
func (b *Broker) attach(s *session, conn *websocket.Conn, isBrowser bool) bool {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return false
	}
	if isBrowser {
		s.browser = conn
	} else {
		s.agent = conn
	}
	both := s.browser != nil && s.agent != nil
	s.mu.Unlock()

	s.pairOnce.Do(func() { go b.pair(s) })
	if both {
		close(s.ready)
	}
	return true
}

// pair waits for the second side, failing the session if it has not arrived
// by the pairing deadline (ticket issue + TicketTTL).
func (b *Broker) pair(s *session) {
	t := time.NewTimer(time.Until(b.pairDeadline(s)))
	defer t.Stop()
	select {
	case <-s.ready:
	case <-s.closed:
		return
	case <-t.C:
		select {
		case <-s.ready: // both sides made it just in time
		default:
			b.fail(s, websocket.StatusTryAgainLater, ReasonPairingTimeout)
			return
		}
	}
	b.run(s)
}

func (b *Broker) run(s *session) {
	// Pairing is the other half of end's atomic transition: under s.mu the
	// session either has already ended (a failure won) or becomes paired, and
	// fail then leaves it alone.
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.paired = true
	s.lastActive = time.Now()
	browser, agent := s.browser, s.agent
	s.mu.Unlock()

	if b.store != nil {
		if err := b.store.InsertTerminalSession(context.Background(), s.ticket.SessionID, s.ticket.DeviceID, s.ticket.User); err != nil {
			b.log.Error("insert terminal session", "session", s.ticket.SessionID, "err", err)
		}
		b.store.Audit(context.Background(), s.ticket.User, "terminal_open", s.ticket.DeviceName, "ok", s.ticket.SessionID)
	}

	done := make(chan struct{}, 2)
	go func() {
		b.relay(s, browser, agent, true)
		done <- struct{}{}
	}()
	go func() {
		b.relay(s, agent, browser, false)
		done <- struct{}{}
	}()
	go b.watchIdle(s)

	<-done
	b.closeSession(s, websocket.StatusNormalClosure, ReasonSessionEnded)

	s.mu.Lock()
	bytesIn, bytesOut, reason := s.bytesIn, s.bytesOut, s.endReason
	s.mu.Unlock()
	if b.store != nil {
		if err := b.store.CloseTerminalSession(context.Background(), s.ticket.SessionID, bytesIn, bytesOut); err != nil {
			b.log.Error("close terminal session", "session", s.ticket.SessionID, "err", err)
		}
		b.store.Audit(context.Background(), s.ticket.User, "terminal_close", s.ticket.DeviceName, "ok",
			s.ticket.SessionID+": "+reason)
	}
}

// watchIdle ends the session once no frame has crossed the relay in either
// direction for IdleTimeout.
func (b *Broker) watchIdle(s *session) {
	check := b.opts.IdleTimeout / 4
	if check > maxIdleCheck {
		check = maxIdleCheck
	}
	t := time.NewTicker(check)
	defer t.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-t.C:
			s.mu.Lock()
			idle := time.Since(s.lastActive)
			s.mu.Unlock()
			if idle >= b.opts.IdleTimeout {
				b.closeSession(s, websocket.StatusGoingAway, ReasonIdleTimeout)
				return
			}
		}
	}
}

// relay copies frames from src to dst until either side fails or closes.
// Reads carry no per-direction deadline (idleness is judged across both
// directions by watchIdle) and no cancellable context: cancelling a pending
// Read hard-closes the socket and would drop the close reason. closeSession
// unblocks them by closing both sockets.
func (b *Broker) relay(s *session, src, dst *websocket.Conn, isBrowserToAgent bool) {
	dir := "agent->browser"
	if isBrowserToAgent {
		dir = "browser->agent"
	}
	for {
		typ, data, err := src.Read(context.Background())
		if err != nil {
			if websocket.CloseStatus(err) == -1 && !s.isEnded() {
				b.log.Info("relay read error", "session", s.ticket.SessionID, "dir", dir, "err", err)
			}
			return
		}
		s.mu.Lock()
		s.lastActive = time.Now()
		if isBrowserToAgent {
			s.bytesIn += int64(len(data))
		} else {
			s.bytesOut += int64(len(data))
		}
		s.mu.Unlock()

		wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = dst.Write(wctx, typ, data)
		wcancel()
		if err != nil {
			if websocket.CloseStatus(err) == -1 && !s.isEnded() {
				b.log.Info("relay write error", "session", s.ticket.SessionID, "dir", dir, "err", err)
			}
			return
		}
	}
}

func (s *session) isEnded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended
}

// fail ends a session that has not paired, closing whichever side is
// connected with reason, and audits the failed open. A paired session is left
// alone (run records its outcome). It reports whether this call ended the
// session.
func (b *Broker) fail(s *session, code websocket.StatusCode, reason string) bool {
	if did, _ := b.end(s, true, code, reason); !did {
		return false
	}
	b.recordFailedOpen(s, reason)
	return true
}

func (b *Broker) recordFailedOpen(s *session, reason string) {
	b.log.Info("terminal open failed", "device", s.ticket.DeviceName, "session", s.ticket.SessionID, "reason", reason)
	if b.store != nil {
		b.store.Audit(context.Background(), s.ticket.User, "terminal_open", s.ticket.DeviceName, "failed",
			s.ticket.SessionID+": "+reason)
	}
}

// closeSession ends the session whatever its state. It reports whether this
// call did the closing.
func (b *Broker) closeSession(s *session, code websocket.StatusCode, reason string) bool {
	did, _ := b.end(s, false, code, reason)
	return did
}

// end is the session's single terminal transition: it marks the session
// ended (recording reason), voids its tickets, frees its device slot and
// closes both sockets. The check and the transition happen under s.mu, the
// same lock run holds while marking the session paired, so a session is
// either ended before it pairs or paired before it ends. With onlyUnpaired
// set, a paired session is left alone. It reports whether this call ended the
// session and whether the session had been paired.
func (b *Broker) end(s *session, onlyUnpaired bool, code websocket.StatusCode, reason string) (did, wasPaired bool) {
	s.mu.Lock()
	wasPaired = s.paired
	if s.ended || (onlyUnpaired && wasPaired) {
		s.mu.Unlock()
		return false, wasPaired
	}
	s.ended = true
	s.endReason = reason
	browser, agent := s.browser, s.agent
	s.mu.Unlock()

	b.removeSession(s)
	close(s.closed)
	if browser != nil {
		go browser.Close(code, reason)
	}
	if agent != nil {
		go agent.Close(code, reason)
	}
	return true, wasPaired
}
