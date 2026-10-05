package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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
	defer b.Close()

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

func newTestBroker(t *testing.T, st *store.Store, opts Options) *Broker {
	t.Helper()
	b := NewBrokerWithOptions(st, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})), opts)
	t.Cleanup(b.Close)
	return b
}

func serveBroker(t *testing.T, b *Broker) string {
	t.Helper()
	srv := httptest.NewServer(b)
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// dialHello opens a terminal socket and sends hello as the first text frame.
func dialHello(t *testing.T, url string, hello map[string]string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	data, _ := json.Marshal(hello)
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
	return conn
}

func browserHello(tk *Ticket) map[string]string {
	return map[string]string{"ticket": tk.BrowserTicket}
}

func agentHello(tk *Ticket) map[string]string {
	return map[string]string{"session_id": tk.SessionID, "agent_ticket": tk.AgentTicket}
}

// expectClose reads until the server closes conn and checks code and reason.
func expectClose(t *testing.T, conn *websocket.Conn, code websocket.StatusCode, reason string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		_, _, err := conn.Read(ctx)
		if err == nil {
			continue
		}
		var ce websocket.CloseError
		if !errors.As(err, &ce) {
			t.Fatalf("want close %d %q, got %v", code, reason, err)
		}
		if ce.Code != code || (reason != "" && ce.Reason != reason) {
			t.Fatalf("want close %d %q, got %d %q", code, reason, ce.Code, ce.Reason)
		}
		return
	}
}

func countAudit(t *testing.T, st *store.Store, action, result string) int {
	t.Helper()
	entries, err := st.ListAudit(context.Background(), 500, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.Action == action && e.Result == result {
			n++
		}
	}
	return n
}

// pairSession connects both sides of tk and waits until the relay is live.
func pairSession(t *testing.T, b *Broker, url string, tk *Ticket) (browser, agent *websocket.Conn) {
	t.Helper()
	browser = dialHello(t, url, browserHello(tk))
	agent = dialHello(t, url, agentHello(tk))
	waitFor(t, func() bool {
		s := b.lookup(tk.SessionID)
		if s == nil {
			return false
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.paired
	})
	return browser, agent
}

func TestPairingTimeoutWithoutAgent(t *testing.T) {
	st := testStore(t)
	d, _, _ := st.CreateDevice(context.Background(), "dev-1", "", false, store.Schedule{})
	b := newTestBroker(t, st, Options{TicketTTL: 100 * time.Millisecond, ReapInterval: time.Hour})
	url := serveBroker(t, b)

	tk, err := b.CreateTicket("dev-1", d.ID, "admin", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	// Fill the second slot too, so freeing is observable at the limit.
	if _, err := b.CreateTicket("dev-1", d.ID, "admin", 80, 24); err != nil {
		t.Fatal(err)
	}

	// The browser connects with a valid ticket; the agent never dials.
	browser := dialHello(t, url, browserHello(tk))
	expectClose(t, browser, websocket.StatusTryAgainLater, ReasonPairingTimeout)

	// Both tickets are void: a late agent dial is rejected.
	late := dialHello(t, url, agentHello(tk))
	expectClose(t, late, websocket.StatusPolicyViolation, "")
	if _, ok := b.ConsumeBrowserTicket(tk.BrowserTicket); ok {
		t.Fatal("browser ticket valid after pairing timeout")
	}

	// The slot is freed without any reaper pass.
	if n := b.ActiveCount("dev-1"); n != 1 {
		t.Fatalf("active = %d, want 1 (only the untouched ticket)", n)
	}
	if _, err := b.CreateTicket("dev-1", d.ID, "admin", 80, 24); err != nil {
		t.Fatalf("slot not freed: %v", err)
	}
	if n := countAudit(t, st, "terminal_open", "failed"); n != 1 {
		t.Fatalf("want 1 failed-open audit, got %d", n)
	}
	if n, _ := st.CountActiveTerminalSessions(context.Background(), d.ID); n != 0 {
		t.Fatalf("unpaired session recorded as open: %d", n)
	}
}

func TestPairingTimeoutWithoutBrowser(t *testing.T) {
	b := newTestBroker(t, nil, Options{TicketTTL: 100 * time.Millisecond, ReapInterval: time.Hour})
	url := serveBroker(t, b)
	tk, _ := b.CreateTicket("dev-1", 42, "admin", 80, 24)

	agent := dialHello(t, url, agentHello(tk))
	expectClose(t, agent, websocket.StatusTryAgainLater, ReasonPairingTimeout)
	if n := b.ActiveCount("dev-1"); n != 0 {
		t.Fatalf("slot not freed: %d", n)
	}
}

func TestAgentRefusalClosesBrowser(t *testing.T) {
	st := testStore(t)
	d, _, _ := st.CreateDevice(context.Background(), "dev-1", "", false, store.Schedule{})
	// Long TTL: voiding must come from the refusal, not from expiry.
	b := newTestBroker(t, st, Options{TicketTTL: time.Minute, ReapInterval: time.Hour})
	url := serveBroker(t, b)
	tk, _ := b.CreateTicket("dev-1", d.ID, "admin", 80, 24)

	browser := dialHello(t, url, browserHello(tk))
	// Wait until the browser is attached so the refusal has a socket to close.
	waitFor(t, func() bool {
		s := b.lookup(tk.SessionID)
		if s == nil {
			return false
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.browser != nil
	})

	if !b.RefuseSession("dev-1", tk.SessionID) {
		t.Fatal("refusal not applied")
	}
	expectClose(t, browser, websocket.StatusPolicyViolation, ReasonAgentRefused)

	late := dialHello(t, url, agentHello(tk))
	expectClose(t, late, websocket.StatusPolicyViolation, "")
	if n := b.ActiveCount("dev-1"); n != 0 {
		t.Fatalf("slot not freed: %d", n)
	}
	if n := countAudit(t, st, "terminal_open", "failed"); n != 1 {
		t.Fatalf("want 1 failed-open audit, got %d", n)
	}
}

func TestPairedSessionCountedOnce(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	d, _, _ := st.CreateDevice(ctx, "dev-1", "", false, store.Schedule{})
	b := newTestBroker(t, st, Options{})
	url := serveBroker(t, b)

	tk, _ := b.CreateTicket("dev-1", d.ID, "admin", 80, 24)
	pairSession(t, b, url, tk)
	waitFor(t, func() bool {
		n, _ := st.CountActiveTerminalSessions(ctx, d.ID)
		return n == 1
	})

	// One live session (in both broker and DB) leaves exactly one slot.
	if n := b.ActiveCount("dev-1"); n != 1 {
		t.Fatalf("active = %d, want 1", n)
	}
	if _, err := b.CreateTicket("dev-1", d.ID, "admin", 80, 24); err != nil {
		t.Fatalf("second session refused: %v", err)
	}
	if _, err := b.CreateTicket("dev-1", d.ID, "admin", 80, 24); err != ErrSessionLimit {
		t.Fatalf("third session: want ErrSessionLimit, got %v", err)
	}
}

func TestSessionCloseRecordsAndFreesSlot(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	d, _, _ := st.CreateDevice(ctx, "dev-1", "", false, store.Schedule{})
	b := newTestBroker(t, st, Options{})
	url := serveBroker(t, b)

	tk, _ := b.CreateTicket("dev-1", d.ID, "admin", 80, 24)
	browser, agent := pairSession(t, b, url, tk)

	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	browser.Write(wctx, websocket.MessageBinary, []byte("ls\n"))
	if _, data, err := agent.Read(wctx); err != nil || string(data) != "ls\n" {
		t.Fatalf("agent read %q %v", data, err)
	}

	// The agent ending closes the browser and records the session.
	agent.Close(websocket.StatusNormalClosure, "exit")
	expectClose(t, browser, websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return countAudit(t, st, "terminal_close", "ok") == 1 })
	if n, _ := st.CountActiveTerminalSessions(ctx, d.ID); n != 0 {
		t.Fatalf("row still open: %d", n)
	}
	if n := b.ActiveCount("dev-1"); n != 0 {
		t.Fatalf("slot not freed: %d", n)
	}
}

func TestIdleTimeoutCountsBothDirections(t *testing.T) {
	const idle = 200 * time.Millisecond
	b := newTestBroker(t, nil, Options{IdleTimeout: idle})
	url := serveBroker(t, b)
	tk, _ := b.CreateTicket("dev-1", 42, "admin", 80, 24)
	browser, agent := pairSession(t, b, url, tk)

	// A passive viewer: only the agent sends, for several idle periods.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop := time.Now().Add(3 * idle)
	for time.Now().Before(stop) {
		if err := agent.Write(ctx, websocket.MessageBinary, []byte("tail line\n")); err != nil {
			t.Fatalf("agent write: %v", err)
		}
		if _, _, err := browser.Read(ctx); err != nil {
			t.Fatalf("passive viewer cut while output flowed: %v", err)
		}
		time.Sleep(idle / 8)
	}

	// No traffic in either direction: the session ends.
	expectClose(t, browser, websocket.StatusGoingAway, ReasonIdleTimeout)
	expectClose(t, agent, websocket.StatusGoingAway, ReasonIdleTimeout)
}

// TestPairingDeadlineFromIssue: the pairing window is measured from ticket
// issue, not from the first side attaching, so a side that attaches late in
// the window waits only for what is left of it.
func TestPairingDeadlineFromIssue(t *testing.T) {
	const ttl = 600 * time.Millisecond
	b := newTestBroker(t, nil, Options{TicketTTL: ttl, ReapInterval: time.Hour})
	url := serveBroker(t, b)
	tk, _ := b.CreateTicket("dev-1", 42, "admin", 80, 24)
	issued := time.Now()

	time.Sleep(ttl * 4 / 5)
	browser := dialHello(t, url, browserHello(tk))
	expectClose(t, browser, websocket.StatusTryAgainLater, ReasonPairingTimeout)

	// Measured from attach this would end near 1.8*ttl.
	if el := time.Since(issued); el < ttl || el > ttl*4/3 {
		t.Fatalf("pairing ended %v after issue, want about %v", el, ttl)
	}
}
