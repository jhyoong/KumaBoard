# Phase 3a: Web Terminal — Backend Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build the server terminal broker, store methods, API wiring, and agent PTY handling for the web terminal feature.

**Architecture:** A new `server/terminal/` package manages tickets, pairing, and relay between browser and agent WebSocket connections. The hub sends `terminal_open` over the existing control socket. The agent spawns a PTY via `creack/pty` and dials back on a separate WebSocket.

**Tech Stack:** Go (creack/pty, coder/websocket), SQLite

---

### Task 1: Store — terminal session CRUD methods

Add the three store methods that the broker will call to track terminal sessions.

**Files:**
- Modify: `server/store/devices.go` (add terminal session methods)
- Modify: `server/store/devices_test.go` (add tests)

**Step 1: Write the failing tests**

Add to `server/store/devices_test.go`:

```go
func TestTerminalSessionLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _, _ := s.CreateDevice(ctx, "deb", "", false, Schedule{})

	if err := s.InsertTerminalSession(ctx, "sess-1", d.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	n, err := s.CountActiveTerminalSessions(ctx, d.ID)
	if err != nil || n != 1 {
		t.Fatalf("count=%d err=%v, want 1", n, err)
	}

	if err := s.InsertTerminalSession(ctx, "sess-2", d.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	n, _ = s.CountActiveTerminalSessions(ctx, d.ID)
	if n != 2 {
		t.Fatalf("count=%d, want 2", n)
	}

	if err := s.CloseTerminalSession(ctx, "sess-1", 1024, 2048); err != nil {
		t.Fatal(err)
	}
	n, _ = s.CountActiveTerminalSessions(ctx, d.ID)
	if n != 1 {
		t.Fatalf("count=%d after close, want 1", n)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `cd /path/to/KumaBoard && go test ./server/store/ -run TestTerminalSessionLifecycle -v`
Expected: FAIL — `InsertTerminalSession` not defined

**Step 3: Write the implementation**

Add to `server/store/devices.go`:

```go
// InsertTerminalSession records the start of a terminal session.
func (s *Store) InsertTerminalSession(ctx context.Context, id string, deviceID int64, user string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO terminal_sessions (id, device_id, user, started_at) VALUES (?, ?, ?, ?)`,
		id, deviceID, user, nowString())
	return err
}

// CloseTerminalSession stamps ended_at and final byte counts.
func (s *Store) CloseTerminalSession(ctx context.Context, id string, bytesIn, bytesOut int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE terminal_sessions SET ended_at = ?, bytes_in = ?, bytes_out = ? WHERE id = ?`,
		nowString(), bytesIn, bytesOut, id)
	return err
}

// CountActiveTerminalSessions returns sessions without an ended_at for a device.
func (s *Store) CountActiveTerminalSessions(ctx context.Context, deviceID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM terminal_sessions WHERE device_id = ? AND ended_at IS NULL`, deviceID).Scan(&n)
	return n, err
}
```

**Step 4: Run test to verify it passes**

Run: `cd /path/to/KumaBoard && go test ./server/store/ -run TestTerminalSessionLifecycle -v`
Expected: PASS

**Step 5: Commit**

```bash
git add server/store/devices.go server/store/devices_test.go
git commit -m "feat(store): terminal session insert, close, and count methods"
```

---

### Task 2: Store — extend UpdateDeviceSettings for terminal_enabled

**Files:**
- Modify: `server/store/devices.go:152-164` (`UpdateDeviceSettings`)
- Modify: `server/store/devices_test.go` (add test)

**Step 1: Write the failing test**

Add to `server/store/devices_test.go`:

```go
func TestUpdateDeviceTerminalEnabled(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	s.CreateDevice(ctx, "deb", "", false, Schedule{})

	if err := s.UpdateDeviceSettings(ctx, "deb", "", false, Schedule{}, true); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDevice(ctx, "deb")
	if !d.TerminalEnabled {
		t.Fatal("terminal_enabled not set")
	}

	if err := s.UpdateDeviceSettings(ctx, "deb", "", false, Schedule{}, false); err != nil {
		t.Fatal(err)
	}
	d, _ = s.GetDevice(ctx, "deb")
	if d.TerminalEnabled {
		t.Fatal("terminal_enabled not cleared")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `cd /path/to/KumaBoard && go test ./server/store/ -run TestUpdateDeviceTerminalEnabled -v`
Expected: FAIL — wrong number of arguments to `UpdateDeviceSettings`

**Step 3: Update the function signature and implementation**

Change `UpdateDeviceSettings` in `server/store/devices.go`:

```go
func (s *Store) UpdateDeviceSettings(ctx context.Context, name, mac string, normallyOff bool, sched Schedule, terminalEnabled bool) error {
	schedJSON, _ := json.Marshal(sched)
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET mac = ?, normally_off = ?, schedule_json = ?, terminal_enabled = ? WHERE name = ?`,
		mac, b2i(normallyOff), string(schedJSON), b2i(terminalEnabled), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
```

**Step 4: Fix all callers**

The only callers are:
- `server/api/devices.go:89` — update to pass `body.TerminalEnabled`
- `server/store/devices_test.go:79` (`TestScheduleRoundTrip`) — add `false` parameter

In `server/api/devices.go`, add `TerminalEnabled` to `deviceBody`:

```go
type deviceBody struct {
	Name                string         `json:"name"`
	MAC                 string         `json:"mac"`
	NormallyOff         bool           `json:"normally_off"`
	TerminalEnabled     bool           `json:"terminal_enabled"`
	Schedule            store.Schedule `json:"schedule"`
	DesiredAgentVersion *string        `json:"desired_agent_version,omitempty"`
}
```

Update the call in `updateDevice`:

```go
if err := s.Store.UpdateDeviceSettings(r.Context(), name, body.MAC, body.NormallyOff, body.Schedule, body.TerminalEnabled); err != nil {
```

Update `TestScheduleRoundTrip` in `server/store/devices_test.go`:

```go
if err := s.UpdateDeviceSettings(ctx, "deb", "11:22:33:44:55:66", true, sched, false); err != nil {
```

**Step 5: Run all tests to verify nothing broke**

Run: `cd /path/to/KumaBoard && go test ./server/store/ ./server/api/ -v`
Expected: all PASS

**Step 6: Commit**

```bash
git add server/store/devices.go server/store/devices_test.go server/api/devices.go
git commit -m "feat(store): add terminal_enabled to UpdateDeviceSettings"
```

---

### Task 3: Server terminal broker — ticket and session management

Create the core `server/terminal/` package with ticket creation, validation, and session state tracking.

**Files:**
- Create: `server/terminal/broker.go`
- Create: `server/terminal/broker_test.go`

**Step 1: Write the failing test**

Create `server/terminal/broker_test.go`:

```go
package terminal

import (
	"testing"
	"time"
)

func TestTicketCreateAndConsume(t *testing.T) {
	b := NewBroker(nil, nil)

	tk, err := b.CreateTicket("dev-1", 42, "admin", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if tk.DeviceName != "dev-1" || tk.Cols != 80 || tk.Rows != 24 {
		t.Fatalf("unexpected ticket: %+v", tk)
	}

	// Browser ticket is valid
	got, ok := b.ConsumeBrowserTicket(tk.BrowserTicket)
	if !ok {
		t.Fatal("browser ticket not found")
	}
	if got.SessionID != tk.SessionID {
		t.Fatal("session ID mismatch")
	}

	// Second consume fails (single-use)
	if _, ok := b.ConsumeBrowserTicket(tk.BrowserTicket); ok {
		t.Fatal("browser ticket reused")
	}
}

func TestAgentTicketConsume(t *testing.T) {
	b := NewBroker(nil, nil)

	tk, _ := b.CreateTicket("dev-1", 42, "admin", 80, 24)

	got, ok := b.ConsumeAgentTicket(tk.SessionID, tk.AgentTicket)
	if !ok || got.SessionID != tk.SessionID {
		t.Fatal("agent ticket not valid")
	}

	// Second consume fails
	if _, ok := b.ConsumeAgentTicket(tk.SessionID, tk.AgentTicket); ok {
		t.Fatal("agent ticket reused")
	}
}

func TestTicketExpiry(t *testing.T) {
	b := NewBroker(nil, nil)
	b.ticketTTL = 1 * time.Millisecond

	tk, _ := b.CreateTicket("dev-1", 42, "admin", 80, 24)
	time.Sleep(5 * time.Millisecond)

	if _, ok := b.ConsumeBrowserTicket(tk.BrowserTicket); ok {
		t.Fatal("expired browser ticket accepted")
	}
	if _, ok := b.ConsumeAgentTicket(tk.SessionID, tk.AgentTicket); ok {
		t.Fatal("expired agent ticket accepted")
	}
}

func TestSessionLimit(t *testing.T) {
	b := NewBroker(nil, nil)

	b.CreateTicket("dev-1", 42, "admin", 80, 24)
	b.CreateTicket("dev-1", 42, "admin", 80, 24)

	_, err := b.CreateTicket("dev-1", 42, "admin", 80, 24)
	if err != ErrSessionLimit {
		t.Fatalf("expected ErrSessionLimit, got %v", err)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `cd /path/to/KumaBoard && go test ./server/terminal/ -v`
Expected: FAIL — package doesn't exist

**Step 3: Write the broker implementation**

Create `server/terminal/broker.go`:

```go
package terminal

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/jhyoong/KumaBoard/server/store"
)

var (
	ErrSessionLimit = errors.New("terminal: session limit reached")
)

const (
	maxSessionsPerDevice = 2
	defaultTicketTTL     = 30 * time.Second
	idleTimeout          = 30 * time.Minute
)

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
	ticket     *Ticket
	browserWS  chan wsConn
	agentWS    chan wsConn
	closed     chan struct{}
	closeOnce  sync.Once
	lastActive time.Time
	bytesIn    int64
	bytesOut   int64
	mu         sync.Mutex
}

type Broker struct {
	store     *store.Store
	log       *slog.Logger
	ticketTTL time.Duration

	mu       sync.Mutex
	sessions map[string]*session // by session ID
}

func NewBroker(st *store.Store, log *slog.Logger) *Broker {
	if log == nil {
		log = slog.Default()
	}
	return &Broker{
		store:     st,
		log:       log,
		ticketTTL: defaultTicketTTL,
		sessions:  map[string]*session{},
	}
}

func (b *Broker) CreateTicket(deviceName string, deviceID int64, user string, cols, rows int) (*Ticket, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	count := 0
	for _, s := range b.sessions {
		if s.ticket.DeviceName == deviceName {
			count++
		}
	}
	if count >= maxSessionsPerDevice {
		return nil, ErrSessionLimit
	}

	tk := &Ticket{
		SessionID:     ulid.Make().String(),
		BrowserTicket: randomToken(),
		AgentTicket:   randomToken(),
		DeviceName:    deviceName,
		DeviceID:      deviceID,
		User:          user,
		Cols:          cols,
		Rows:          rows,
		CreatedAt:     time.Now(),
	}
	b.sessions[tk.SessionID] = &session{
		ticket:     tk,
		browserWS:  make(chan wsConn, 1),
		agentWS:    make(chan wsConn, 1),
		closed:     make(chan struct{}),
		lastActive: time.Now(),
	}
	return tk, nil
}

func (b *Broker) ConsumeBrowserTicket(ticket string) (*Ticket, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, s := range b.sessions {
		if s.ticket.BrowserTicket != "" && s.ticket.BrowserTicket == ticket {
			if time.Since(s.ticket.CreatedAt) > b.ticketTTL {
				delete(b.sessions, s.ticket.SessionID)
				return nil, false
			}
			s.ticket.BrowserTicket = "" // consume
			return s.ticket, true
		}
	}
	return nil, false
}

func (b *Broker) ConsumeAgentTicket(sessionID, ticket string) (*Ticket, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	s, ok := b.sessions[sessionID]
	if !ok {
		return nil, false
	}
	if s.ticket.AgentTicket == "" || s.ticket.AgentTicket != ticket {
		return nil, false
	}
	if time.Since(s.ticket.CreatedAt) > b.ticketTTL {
		delete(b.sessions, sessionID)
		return nil, false
	}
	s.ticket.AgentTicket = "" // consume
	return s.ticket, true
}

func (b *Broker) getSession(sessionID string) *session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions[sessionID]
}

func (b *Broker) removeSession(sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sessions, sessionID)
}

func (b *Broker) ActiveCount(deviceName string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
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
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
```

**Step 4: Run tests to verify they pass**

Run: `cd /path/to/KumaBoard && go test ./server/terminal/ -v`
Expected: PASS (the `wsConn` type doesn't exist yet — add a placeholder)

Note: The `wsConn` type will be defined in Task 4. For now, declare it as an interface at the top of `broker.go`:

```go
type wsConn interface {
	Close(code int, reason string) error
}
```

**Step 5: Commit**

```bash
git add server/terminal/
git commit -m "feat(terminal): broker with ticket creation, validation, and session limits"
```

---

### Task 4: Server terminal broker — WebSocket relay

Add the WebSocket handler that accepts browser and agent connections, pairs them, and relays frames.

**Files:**
- Modify: `server/terminal/broker.go` (add `ServeHTTP`, relay logic)
- Create: `server/terminal/relay_test.go` (integration test with two websocket clients)

**Step 1: Write the failing test**

Create `server/terminal/relay_test.go`:

```go
package terminal

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jhyoong/KumaBoard/server/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestRelayBinaryFrames(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	d, _, _ := st.CreateDevice(ctx, "dev-1", "", false, store.Schedule{})
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	b := NewBroker(st, log)

	srv := httptest.NewServer(b)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	tk, err := b.CreateTicket("dev-1", d.ID, "admin", 80, 24)
	if err != nil {
		t.Fatal(err)
	}

	// Browser connects and sends ticket
	bctx, bcancel := context.WithTimeout(ctx, 5*time.Second)
	defer bcancel()
	browserConn, _, err := websocket.Dial(bctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer browserConn.CloseNow()
	browserHello, _ := json.Marshal(map[string]string{"ticket": tk.BrowserTicket})
	browserConn.Write(bctx, websocket.MessageText, browserHello)

	// Agent connects and sends hello
	actx, acancel := context.WithTimeout(ctx, 5*time.Second)
	defer acancel()
	agentConn, _, err := websocket.Dial(actx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer agentConn.CloseNow()
	agentHello, _ := json.Marshal(map[string]string{
		"session_id":   tk.SessionID,
		"agent_ticket": tk.AgentTicket,
	})
	agentConn.Write(actx, websocket.MessageText, agentHello)

	// Agent writes binary, browser should read it
	time.Sleep(50 * time.Millisecond) // let pairing complete
	agentConn.Write(actx, websocket.MessageBinary, []byte("hello from agent"))

	rctx, rcancel := context.WithTimeout(ctx, 2*time.Second)
	defer rcancel()
	typ, data, err := browserConn.Read(rctx)
	if err != nil {
		t.Fatal("browser read:", err)
	}
	if typ != websocket.MessageBinary || string(data) != "hello from agent" {
		t.Fatalf("browser got type=%v data=%q", typ, data)
	}

	// Browser writes binary, agent should read it
	browserConn.Write(bctx, websocket.MessageBinary, []byte("hello from browser"))

	rctx2, rcancel2 := context.WithTimeout(ctx, 2*time.Second)
	defer rcancel2()
	typ, data, err = agentConn.Read(rctx2)
	if err != nil {
		t.Fatal("agent read:", err)
	}
	if typ != websocket.MessageBinary || string(data) != "hello from browser" {
		t.Fatalf("agent got type=%v data=%q", typ, data)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `cd /path/to/KumaBoard && go test ./server/terminal/ -run TestRelayBinaryFrames -v`
Expected: FAIL — `Broker` doesn't implement `http.Handler`

**Step 3: Implement ServeHTTP and relay**

Replace the `wsConn` placeholder in `broker.go` and add the HTTP handler and relay logic:

```go
import (
	// add these to existing imports
	"context"
	"encoding/json"

	"github.com/coder/websocket"
)

// Remove the wsConn interface placeholder if present, and update the session struct:

type session struct {
	ticket     *Ticket
	browserCh  chan *websocket.Conn
	agentCh    chan *websocket.Conn
	closed     chan struct{}
	closeOnce  sync.Once
	lastActive time.Time
	bytesIn    int64
	bytesOut   int64
	mu         sync.Mutex
}

// Update CreateTicket to use the new channel types:
// browserWS -> browserCh, agentWS -> agentCh with type chan *websocket.Conn

func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(64 << 10) // 64 KiB per frame

	ctx, cancel := context.WithTimeout(r.Context(), b.ticketTTL)
	defer cancel()
	typ, data, err := conn.Read(ctx)
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
		b.handleBrowser(r.Context(), conn, hello.Ticket)
	} else if hello.SessionID != "" {
		b.handleAgent(r.Context(), conn, hello.SessionID, hello.AgentTicket)
	} else {
		conn.Close(websocket.StatusProtocolError, "missing ticket or session_id")
	}
}

func (b *Broker) handleBrowser(ctx context.Context, conn *websocket.Conn, ticket string) {
	tk, ok := b.ConsumeBrowserTicket(ticket)
	if !ok {
		conn.Close(websocket.StatusPolicyViolation, "invalid or expired ticket")
		return
	}
	s := b.getSession(tk.SessionID)
	if s == nil {
		conn.Close(websocket.StatusPolicyViolation, "session not found")
		return
	}
	select {
	case s.browserCh <- conn:
	default:
		conn.Close(websocket.StatusPolicyViolation, "browser already connected")
		return
	}
	<-s.closed
}

func (b *Broker) handleAgent(ctx context.Context, conn *websocket.Conn, sessionID, ticket string) {
	tk, ok := b.ConsumeAgentTicket(sessionID, ticket)
	if !ok {
		conn.Close(websocket.StatusPolicyViolation, "invalid or expired agent ticket")
		return
	}
	s := b.getSession(tk.SessionID)
	if s == nil {
		conn.Close(websocket.StatusPolicyViolation, "session not found")
		return
	}
	select {
	case s.agentCh <- conn:
	default:
		conn.Close(websocket.StatusPolicyViolation, "agent already connected")
		return
	}
	// Start relay once both sides are present
	go b.tryRelay(s)
	<-s.closed
}

func (b *Broker) tryRelay(s *session) {
	var browser, agent *websocket.Conn

	timeout := time.NewTimer(b.ticketTTL)
	defer timeout.Stop()

	// Wait for both sides
	for browser == nil || agent == nil {
		select {
		case c := <-s.browserCh:
			browser = c
		case c := <-s.agentCh:
			agent = c
		case <-timeout.C:
			b.closeSession(s, browser, agent, websocket.StatusGoingAway, "pairing timeout")
			return
		}
	}

	// Record session open
	if b.store != nil {
		b.store.InsertTerminalSession(context.Background(), s.ticket.SessionID, s.ticket.DeviceID, s.ticket.User)
		b.store.Audit(context.Background(), s.ticket.User, "terminal_open", s.ticket.DeviceName, "ok", s.ticket.SessionID)
	}

	done := make(chan struct{}, 2)
	// browser -> agent
	go func() {
		b.relay(s, browser, agent, true)
		done <- struct{}{}
	}()
	// agent -> browser
	go func() {
		b.relay(s, agent, browser, false)
		done <- struct{}{}
	}()

	// Idle timeout
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.closed:
				return
			case <-t.C:
				s.mu.Lock()
				idle := time.Since(s.lastActive)
				s.mu.Unlock()
				if idle > idleTimeout {
					b.closeSession(s, browser, agent, websocket.StatusGoingAway, "idle timeout")
					return
				}
			}
		}
	}()

	<-done
	b.closeSession(s, browser, agent, websocket.StatusNormalClosure, "session ended")

	// Record session close
	s.mu.Lock()
	bytesIn, bytesOut := s.bytesIn, s.bytesOut
	s.mu.Unlock()
	if b.store != nil {
		b.store.CloseTerminalSession(context.Background(), s.ticket.SessionID, bytesIn, bytesOut)
		b.store.Audit(context.Background(), s.ticket.User, "terminal_close", s.ticket.DeviceName, "ok", s.ticket.SessionID)
	}
}

func (b *Broker) relay(s *session, src, dst *websocket.Conn, isBrowserToAgent bool) {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), idleTimeout)
		typ, data, err := src.Read(ctx)
		cancel()
		if err != nil {
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
			return
		}
	}
}

func (b *Broker) closeSession(s *session, browser, agent *websocket.Conn, code websocket.StatusCode, reason string) {
	s.closeOnce.Do(func() {
		close(s.closed)
		if browser != nil {
			go browser.Close(code, reason)
		}
		if agent != nil {
			go agent.Close(code, reason)
		}
		b.removeSession(s.ticket.SessionID)
	})
}
```

Also update `CreateTicket` to use the new channel types:

```go
b.sessions[tk.SessionID] = &session{
	ticket:     tk,
	browserCh:  make(chan *websocket.Conn, 1),
	agentCh:    make(chan *websocket.Conn, 1),
	closed:     make(chan struct{}),
	lastActive: time.Now(),
}
```

**Step 4: Run tests to verify they pass**

Run: `cd /path/to/KumaBoard && go test ./server/terminal/ -v`
Expected: all PASS

**Step 5: Commit**

```bash
git add server/terminal/
git commit -m "feat(terminal): WebSocket relay between browser and agent"
```

---

### Task 5: Wire broker into server — ticket endpoint and WebSocket mount

**Files:**
- Create: `server/api/terminal.go`
- Modify: `server/api/api.go` (add `Broker` dep, mount terminal endpoint)
- Modify: `cmd/kumaboard/serve.go` (create broker, pass to deps, mount `/ws/terminal`)

**Step 1: Create the terminal API handler**

Create `server/api/terminal.go`:

```go
package api

import (
	"net/http"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
	"github.com/jhyoong/KumaBoard/server/terminal"
)

func (s *server) requestTerminal(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}
	if body.Cols <= 0 {
		body.Cols = 80
	}
	if body.Rows <= 0 {
		body.Rows = 24
	}

	d, err := s.Store.GetDevice(r.Context(), name)
	if err != nil {
		s.notFoundOr500(w, err)
		return
	}
	if !d.TerminalEnabled {
		writeError(w, http.StatusForbidden, "terminal_not_enabled")
		return
	}
	hasCap := false
	for _, c := range d.Capabilities {
		if c == proto.CapTerminal {
			hasCap = true
			break
		}
	}
	if !hasCap {
		writeError(w, http.StatusForbidden, "terminal_not_capable")
		return
	}
	if !s.Hub.Connected(name) {
		writeError(w, http.StatusConflict, "not_connected")
		return
	}

	n, _ := s.Store.CountActiveTerminalSessions(r.Context(), d.ID)
	brokerCount := s.Terminal.ActiveCount(name)
	if n+brokerCount >= 2 {
		writeError(w, http.StatusConflict, "session_limit")
		return
	}

	tk, err := s.Terminal.CreateTicket(name, d.ID, "admin", body.Cols, body.Rows)
	if err != nil {
		if err == terminal.ErrSessionLimit {
			writeError(w, http.StatusConflict, "session_limit")
			return
		}
		s.Log.Error("create terminal ticket", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	env, _ := proto.New(proto.TypeTerminalOpen, proto.TerminalOpen{
		SessionID:   tk.SessionID,
		AgentTicket: tk.AgentTicket,
		Cols:        body.Cols,
		Rows:        body.Rows,
	})
	sess := s.Hub.Session(name)
	if sess == nil {
		writeError(w, http.StatusConflict, "not_connected")
		return
	}
	if err := sess.Send(env); err != nil {
		writeError(w, http.StatusBadGateway, "agent_unreachable")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"ticket":     tk.BrowserTicket,
		"session_id": tk.SessionID,
	})
}
```

**Step 2: Add `Terminal` to `Deps` and `Session` method to Hub**

In `server/api/api.go`, add to `Deps`:

```go
Terminal *terminal.Broker
```

Add import for `"github.com/jhyoong/KumaBoard/server/terminal"`.

Mount the endpoint inside the `authed` mux:

```go
authed.HandleFunc("POST /api/devices/{name}/terminal", s.requestTerminal)
```

In `server/hub/hub.go`, export the `session` method:

```go
// Session returns the live session for a device, or nil.
func (h *Hub) Session(name string) *Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[name]
}
```

Remove the unexported `session` method (rename to `Session`). Update the one caller (`handleUpgradeResult` or `evaluateUpgrade` etc.) — actually, the existing lowercase `session` is already used internally. Keep both or rename the lowercase one. Simplest: rename all internal calls from `h.session(name)` to `h.Session(name)` and delete the lowercase version.

**Step 3: Wire broker into serve.go**

In `cmd/kumaboard/serve.go`, inside `buildHandler`:

```go
import "github.com/jhyoong/KumaBoard/server/terminal"

// after creating hub h:
termBroker := terminal.NewBroker(st, log)

// add to api.Deps:
Terminal: termBroker,

// add to the root mux, before the catch-all:
mux.Handle("GET /ws/terminal", termBroker)
```

**Step 4: Handle terminal_open_result in hub**

In `server/hub/hub.go`, add a case in `handleMessage`:

```go
case proto.TypeTerminalOpenResult:
	var res proto.TerminalOpenResult
	if err := env.Unmarshal(&res); err != nil {
		return
	}
	if res.Result != "ok" {
		h.opts.Log.Info("terminal refused by agent", "device", s.DeviceName, "session", res.SessionID)
	}
```

**Step 5: Build and test**

Run: `cd /path/to/KumaBoard && go build ./...`
Expected: compiles without errors

Run: `cd /path/to/KumaBoard && go test ./server/... -v`
Expected: all PASS

**Step 6: Commit**

```bash
git add server/api/terminal.go server/api/api.go server/hub/hub.go cmd/kumaboard/serve.go
git commit -m "feat(api): terminal ticket endpoint and WebSocket mount"
```

---

### Task 6: Agent — handle terminal_open and PTY (Unix)

**Files:**
- Create: `agent/terminal/pty_unix.go` (build tag `!windows`)
- Create: `agent/terminal/pty_windows.go` (build tag `windows`)
- Modify: `agent/app/app.go` (handle `TypeTerminalOpen`)
- Modify: `go.mod` (add `creack/pty`)

**Step 1: Add creack/pty dependency**

Run: `cd /path/to/KumaBoard && go get github.com/creack/pty`

**Step 2: Create the Unix PTY handler**

Create `agent/terminal/pty_unix.go`:

```go
//go:build !windows

package terminal

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"

	"github.com/jhyoong/KumaBoard/proto"
)

type Session struct {
	URL       string
	TLS       *tls.Config
	SessionID string
	Ticket    string
	Cols      int
	Rows      int
	Log       *slog.Logger
}

func (s *Session) Run(ctx context.Context) error {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := exec.CommandContext(ctx, shell)
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: uint16(s.Cols),
		Rows: uint16(s.Rows),
	})
	if err != nil {
		return fmt.Errorf("terminal: start pty: %w", err)
	}
	defer ptmx.Close()

	dctx, dcancel := context.WithTimeout(ctx, 15*time.Second)
	conn, _, err := websocket.Dial(dctx, s.URL, &websocket.DialOptions{
		HTTPClient: &http.Client{
			Transport: &http.Transport{TLSClientConfig: s.TLS},
		},
	})
	dcancel()
	if err != nil {
		killPG(cmd)
		return fmt.Errorf("terminal: dial: %w", err)
	}
	defer conn.CloseNow()

	hello, _ := json.Marshal(proto.TerminalHello{
		SessionID:   s.SessionID,
		AgentTicket: s.Ticket,
	})
	wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
	err = conn.Write(wctx, websocket.MessageText, hello)
	wcancel()
	if err != nil {
		killPG(cmd)
		return fmt.Errorf("terminal: write hello: %w", err)
	}

	done := make(chan struct{}, 2)

	// PTY -> WebSocket
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
				werr := conn.Write(wctx, websocket.MessageBinary, buf[:n])
				wcancel()
				if werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// WebSocket -> PTY
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			switch typ {
			case websocket.MessageBinary:
				if _, err := ptmx.Write(data); err != nil {
					return
				}
			case websocket.MessageText:
				var ctrl proto.TerminalControl
				if err := json.Unmarshal(data, &ctrl); err != nil {
					continue
				}
				if ctrl.Type == "resize" {
					pty.Setsize(ptmx, &pty.Winsize{
						Cols: uint16(ctrl.Cols),
						Rows: uint16(ctrl.Rows),
					})
				}
			}
		}
	}()

	<-done

	killPG(cmd)
	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			exitCode = exit.ExitCode()
		}
	}

	exitMsg, _ := json.Marshal(proto.TerminalControl{Type: "exit", Code: exitCode})
	ectx, ecancel := context.WithTimeout(context.Background(), 5*time.Second)
	conn.Write(ectx, websocket.MessageText, exitMsg)
	ecancel()

	conn.Close(websocket.StatusNormalClosure, "shell exited")
	return nil
}

func killPG(cmd *exec.Cmd) {
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
```

**Step 3: Create the Windows stub**

Create `agent/terminal/pty_windows.go`:

```go
//go:build windows

package terminal

import (
	"context"
	"errors"
)

type Session struct{}

func (s *Session) Run(ctx context.Context) error {
	return errors.New("terminal: not supported on windows")
}
```

**Step 4: Handle terminal_open in the agent app**

In `agent/app/app.go`, add the `TypeTerminalOpen` case inside `OnMessage`:

```go
case proto.TypeTerminalOpen:
	var req proto.TerminalOpen
	if err := env.Unmarshal(&req); err != nil {
		return
	}
	if !a.has(proto.CapTerminal) {
		res := proto.TerminalOpenResult{SessionID: req.SessionID, Result: "refused"}
		if reply, err := proto.Reply(env, proto.TypeTerminalOpenResult, res); err == nil {
			send.Send(reply)
		}
		return
	}
	res := proto.TerminalOpenResult{SessionID: req.SessionID, Result: "ok"}
	if reply, err := proto.Reply(env, proto.TypeTerminalOpenResult, res); err == nil {
		send.Send(reply)
	}
	go a.runTerminal(req)
```

Add the `runTerminal` method and the necessary plumbing. The agent needs the server's WebSocket URL for the terminal socket. Derive it from the control socket URL:

```go
func (a *App) runTerminal(req proto.TerminalOpen) {
	base := a.cfg.Server.URL
	base = strings.TrimSuffix(base, "/ws")
	termURL := base + "/ws/terminal"

	sess := &agentterm.Session{
		URL:       termURL,
		TLS:       &tls.Config{RootCAs: a.cfg.CAPool, MinVersion: tls.VersionTLS12},
		SessionID: req.SessionID,
		Ticket:    req.AgentTicket,
		Cols:      req.Cols,
		Rows:      req.Rows,
		Log:       a.log,
	}
	if err := sess.Run(context.Background()); err != nil {
		a.log.Error("terminal session failed", "session", req.SessionID, "err", err)
	}
}
```

Add import alias:

```go
agentterm "github.com/jhyoong/KumaBoard/agent/terminal"
```

**Step 5: Build**

Run: `cd /path/to/KumaBoard && go build ./...`
Expected: compiles

**Step 6: Commit**

```bash
git add agent/terminal/ agent/app/app.go go.mod go.sum
git commit -m "feat(agent): PTY terminal session with Unix support, Windows stub"
```
