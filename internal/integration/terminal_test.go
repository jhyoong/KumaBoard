package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jhyoong/KumaBoard/agent/app"
	"github.com/jhyoong/KumaBoard/agent/transport"
	"github.com/jhyoong/KumaBoard/agent/upgrade"
	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/auth"
	"github.com/jhyoong/KumaBoard/server/store"
	"github.com/jhyoong/KumaBoard/server/terminal"
)

// TestTerminalRoundTrip verifies that an authenticated terminal ticket
// request against a connected, terminal-capable device succeeds end-to-end:
// device registration, agent connection, enabling terminal_enabled, and the
// POST /api/devices/{name}/terminal handshake with the hub.
func TestTerminalRoundTrip(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("term-dev")

	cfg := h.agentConfig("term-dev", token)
	cfg.Capabilities = append(cfg.Capabilities, proto.CapTerminal)
	h.startAgent(cfg, nil)
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("term-dev") })

	if err := h.st.UpdateDeviceSettings(context.Background(), "term-dev", "", false, store.Schedule{}, true); err != nil {
		t.Fatal(err)
	}

	cookie := h.loginCookie()

	reqBody, _ := json.Marshal(map[string]int{"cols": 80, "rows": 24})
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/api/devices/term-dev/terminal", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var out struct {
		Ticket    string `json:"ticket"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Ticket == "" || out.SessionID == "" {
		t.Fatalf("missing ticket/session_id in response: %+v", out)
	}
}

func TestTerminalRequiresAuth(t *testing.T) {
	h := newHarness(t)
	h.registerDevice("term-dev")

	reqBody, _ := json.Marshal(map[string]int{"cols": 80, "rows": 24})
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/api/devices/term-dev/terminal", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// userCookie creates an operator account with the given name and returns an
// authenticated session cookie for it.
func (h *harness) userCookie(name string) *http.Cookie {
	h.t.Helper()
	ctx := context.Background()
	hash, err := auth.HashPassword("pass")
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.UpsertUser(ctx, name, hash); err != nil {
		h.t.Fatal(err)
	}
	uid, _, err := h.st.GetUser(ctx, name)
	if err != nil {
		h.t.Fatal(err)
	}
	sid, exp, err := h.sessions.Create(ctx, uid)
	if err != nil {
		h.t.Fatal(err)
	}
	return &http.Cookie{Name: auth.CookieName, Value: sid, Expires: exp}
}

// requestTicket POSTs a terminal ticket request and returns the status and
// browser ticket.
func (h *harness) requestTicket(cookie *http.Cookie, device string) (int, string) {
	h.t.Helper()
	reqBody, _ := json.Marshal(map[string]int{"cols": 80, "rows": 24})
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/devices/"+device+"/terminal", bytes.NewReader(reqBody))
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Ticket string `json:"ticket"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.Ticket
}

// dialBrowser opens /ws/terminal as the browser and sends the ticket.
func (h *harness) dialBrowser(ticket string) *websocket.Conn {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "wss://"+h.addr+"/ws/terminal", &websocket.DialOptions{HTTPClient: h.srv.Client()})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { conn.CloseNow() })
	hello, _ := json.Marshal(map[string]string{"ticket": ticket})
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		h.t.Fatal(err)
	}
	return conn
}

func (h *harness) auditCount(actor, action, result, detailSub string) int {
	h.t.Helper()
	entries, err := h.st.ListAudit(context.Background(), 500, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.Actor == actor && e.Action == action && e.Result == result && strings.Contains(e.Detail, detailSub) {
			n++
		}
	}
	return n
}

// TestTerminalSessionLimitCountsPairedOnce checks the per-device limit is
// exactly two when one session is live (present in both the broker and the
// terminal_sessions table), and that the session user is the logged-in
// operator rather than a hard-coded name.
func TestTerminalSessionLimitCountsPairedOnce(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("term-dev")
	agentTerms := h.startFakeTerminalAgent("term-dev", token)
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("term-dev") })
	if err := h.st.UpdateDeviceSettings(context.Background(), "term-dev", "", false, store.Schedule{}, true); err != nil {
		t.Fatal(err)
	}
	d, _ := h.st.GetDevice(context.Background(), "term-dev")
	cookie := h.userCookie("alice")

	code, ticket := h.requestTicket(cookie, "term-dev")
	if code != http.StatusOK {
		t.Fatalf("first ticket: %d", code)
	}
	browser := h.dialBrowser(ticket)
	agentTerm := <-agentTerms
	waitFor(t, 3*time.Second, func() bool {
		n, _ := h.st.CountActiveTerminalSessions(context.Background(), d.ID)
		return n == 1
	})
	// The session is live: bytes relay end to end.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	agentTerm.Write(ctx, websocket.MessageBinary, []byte("$ "))
	if _, data, err := browser.Read(ctx); err != nil || string(data) != "$ " {
		t.Fatalf("browser read %q %v", data, err)
	}
	if n := h.auditCount("alice", "terminal_open", "ok", ""); n != 1 {
		t.Fatalf("terminal_open ok audits by alice = %d, want 1", n)
	}

	if code, _ := h.requestTicket(cookie, "term-dev"); code != http.StatusOK {
		t.Fatalf("second ticket with one live session: %d, want 200", code)
	}
	if code, _ := h.requestTicket(cookie, "term-dev"); code != http.StatusConflict {
		t.Fatalf("third ticket: %d, want 409", code)
	}
	if n := h.auditCount("alice", "terminal_open", "failed", "session limit"); n != 1 {
		t.Fatalf("over-limit audits = %d, want 1", n)
	}
}

// startFakeTerminalAgent connects a minimal agent to /ws that declares the
// terminal capability, answers pings, accepts every terminal_open and dials
// /ws/terminal with the agent ticket, without spawning a PTY. Each paired
// agent-side terminal socket is delivered on the returned channel.
func (h *harness) startFakeTerminalAgent(name, token string) <-chan *websocket.Conn {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	client := h.srv.Client()
	conn, _, err := websocket.Dial(ctx, "wss://"+h.addr+"/ws", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { conn.CloseNow() })
	conn.SetReadLimit(proto.MaxMessageSize)
	send := func(env *proto.Envelope) {
		b, _ := proto.Encode(env)
		conn.Write(ctx, websocket.MessageText, b)
	}
	hello, _ := proto.New(proto.TypeHello, proto.Hello{
		ProtocolVersion: proto.Version, AgentVersion: "0.1.0", DeviceName: name, Token: token,
		OS: "linux", Arch: "amd64", Capabilities: []string{proto.CapMetrics, proto.CapTerminal},
	})
	send(hello)

	terms := make(chan *websocket.Conn, 4)
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			env, err := proto.Decode(data)
			if err != nil {
				continue
			}
			switch env.Type {
			case proto.TypePing:
				pong, _ := proto.Reply(env, proto.TypePong, nil)
				send(pong)
			case proto.TypeTerminalOpen:
				var req proto.TerminalOpen
				env.Unmarshal(&req)
				res, _ := proto.Reply(env, proto.TypeTerminalOpenResult,
					proto.TerminalOpenResult{SessionID: req.SessionID, Result: "ok"})
				send(res)
				tc, _, err := websocket.Dial(ctx, "wss://"+h.addr+"/ws/terminal", &websocket.DialOptions{HTTPClient: client})
				if err != nil {
					continue
				}
				th, _ := json.Marshal(proto.TerminalHello{SessionID: req.SessionID, AgentTicket: req.AgentTicket})
				tc.Write(ctx, websocket.MessageText, th)
				terms <- tc
			}
		}
	}()
	return terms
}

// TestTerminalAgentRefusal drives a real agent that advertises the terminal
// capability to the server but refuses terminal_open locally. The refusal
// must void the ticket, free the device slot and be audited.
func TestTerminalAgentRefusal(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("term-dev")
	cfg := h.agentConfig("term-dev", token) // no CapTerminal locally
	a := app.New(cfg, h.log, upgrade.StartupResult{})
	h.startApp(a, cfg, func(c *transport.Client) {
		c.Hello = func() proto.Hello {
			hello := a.Hello()
			hello.Capabilities = append(hello.Capabilities, proto.CapTerminal)
			return hello
		}
	})
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("term-dev") })
	if err := h.st.UpdateDeviceSettings(context.Background(), "term-dev", "", false, store.Schedule{}, true); err != nil {
		t.Fatal(err)
	}
	cookie := h.loginCookie()

	code, ticket := h.requestTicket(cookie, "term-dev")
	if code != http.StatusOK {
		t.Fatalf("ticket: %d", code)
	}
	waitFor(t, 3*time.Second, func() bool { return h.termBroker.ActiveCount("term-dev") == 0 })
	if n := h.auditCount("admin", "terminal_open", "failed", "agent refused"); n != 1 {
		t.Fatalf("refusal audits = %d, want 1", n)
	}

	// The voided ticket is rejected on the browser socket.
	conn := h.dialBrowser(ticket)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("browser with refused ticket: %v", err)
	}

	// Both slots are free again.
	for i := 0; i < 2; i++ {
		if code, _ := h.requestTicket(cookie, "term-dev"); code != http.StatusOK {
			t.Fatalf("ticket %d after refusal: %d", i, code)
		}
	}
}

// --- Phase 3 end-to-end acceptance ---

// enableTerminal flips terminal_enabled on for a registered device.
func (h *harness) enableTerminal(device string) {
	h.t.Helper()
	if err := h.st.UpdateDeviceSettings(context.Background(), device, "", false, store.Schedule{}, true); err != nil {
		h.t.Fatal(err)
	}
}

// startManualTerminalAgent connects a minimal terminal-capable agent to /ws
// that answers pings and replies ok to every terminal_open without dialing
// /ws/terminal. Each request is delivered on the returned channel so the
// test decides when (and whether) the agent side connects.
func (h *harness) startManualTerminalAgent(name, token string) <-chan proto.TerminalOpen {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, "wss://"+h.addr+"/ws", &websocket.DialOptions{HTTPClient: h.srv.Client()})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { conn.CloseNow() })
	conn.SetReadLimit(proto.MaxMessageSize)
	send := func(env *proto.Envelope) {
		b, _ := proto.Encode(env)
		conn.Write(ctx, websocket.MessageText, b)
	}
	hello, _ := proto.New(proto.TypeHello, proto.Hello{
		ProtocolVersion: proto.Version, AgentVersion: "0.1.0", DeviceName: name, Token: token,
		OS: "linux", Arch: "amd64", Capabilities: []string{proto.CapMetrics, proto.CapTerminal},
	})
	send(hello)

	opens := make(chan proto.TerminalOpen, 8)
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			env, err := proto.Decode(data)
			if err != nil {
				continue
			}
			switch env.Type {
			case proto.TypePing:
				pong, _ := proto.Reply(env, proto.TypePong, nil)
				send(pong)
			case proto.TypeTerminalOpen:
				var req proto.TerminalOpen
				env.Unmarshal(&req)
				res, _ := proto.Reply(env, proto.TypeTerminalOpenResult,
					proto.TerminalOpenResult{SessionID: req.SessionID, Result: "ok"})
				send(res)
				opens <- req
			}
		}
	}()
	return opens
}

// dialTerminal opens /ws/terminal and writes hello as the first text frame.
func (h *harness) dialTerminal(hello any) *websocket.Conn {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "wss://"+h.addr+"/ws/terminal", &websocket.DialOptions{HTTPClient: h.srv.Client()})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { conn.CloseNow() })
	b, _ := json.Marshal(hello)
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		h.t.Fatal(err)
	}
	return conn
}

// dialAgentTerminal opens /ws/terminal as the agent side of a session.
func (h *harness) dialAgentTerminal(req proto.TerminalOpen) *websocket.Conn {
	h.t.Helper()
	return h.dialTerminal(proto.TerminalHello{SessionID: req.SessionID, AgentTicket: req.AgentTicket})
}

// recvOpen waits for the next terminal_open delivered to a manual agent.
func recvOpen(t *testing.T, opens <-chan proto.TerminalOpen) proto.TerminalOpen {
	t.Helper()
	select {
	case req := <-opens:
		return req
	case <-time.After(3 * time.Second):
		t.Fatal("no terminal_open reached the agent")
		return proto.TerminalOpen{}
	}
}

// expectClose reads from conn until it fails and asserts the close frame
// carries code and reason. Data frames received first are discarded.
func expectClose(t *testing.T, conn *websocket.Conn, code websocket.StatusCode, reason string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := conn.Read(ctx)
		if err == nil {
			continue
		}
		var ce websocket.CloseError
		if !errors.As(err, &ce) {
			t.Fatalf("read ended without a close frame: %v (want %d %q)", err, code, reason)
		}
		if ce.Code != code || ce.Reason != reason {
			t.Fatalf("close = %d %q, want %d %q", ce.Code, ce.Reason, code, reason)
		}
		return
	}
}

// relay writes payload on src and asserts dst receives it byte for byte as a
// binary message.
func relay(t *testing.T, src, dst *websocket.Conn, payload []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := src.Write(ctx, websocket.MessageBinary, payload); err != nil {
		t.Fatal(err)
	}
	typ, got, err := dst.Read(ctx)
	if err != nil {
		t.Fatalf("relay read: %v", err)
	}
	if typ != websocket.MessageBinary || !bytes.Equal(got, payload) {
		t.Fatalf("relayed %v %q, want binary %q", typ, got, payload)
	}
}

type terminalRow struct {
	User              string
	Ended             bool
	BytesIn, BytesOut int64
}

// terminalSession reads one terminal_sessions row through a separate
// connection to the harness database; ok is false if the row does not exist.
func (h *harness) terminalSession(id string) (terminalRow, bool) {
	h.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(h.dir, "kb.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		h.t.Fatal(err)
	}
	defer db.Close()
	var r terminalRow
	var ended sql.NullString
	err = db.QueryRow(`SELECT user, ended_at, bytes_in, bytes_out FROM terminal_sessions WHERE id = ?`, id).
		Scan(&r.User, &ended, &r.BytesIn, &r.BytesOut)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	r.Ended = ended.Valid && ended.String != ""
	return r, true
}

// pairSession requests a ticket through the dashboard API, connects the
// browser, then the agent, and waits until the broker has paired them (the
// terminal_sessions row exists).
func (h *harness) pairSession(cookie *http.Cookie, device string, opens <-chan proto.TerminalOpen) (browser, agent *websocket.Conn, sessionID string) {
	h.t.Helper()
	code, ticket := h.requestTicket(cookie, device)
	if code != http.StatusOK {
		h.t.Fatalf("ticket: %d", code)
	}
	req := recvOpen(h.t, opens)
	browser = h.dialBrowser(ticket)
	agent = h.dialAgentTerminal(req)
	waitFor(h.t, 3*time.Second, func() bool {
		_, ok := h.terminalSession(req.SessionID)
		return ok
	})
	return browser, agent, req.SessionID
}

// allBytes returns every byte value n times, to prove the relay is binary-clean.
func allBytes(n int) []byte {
	b := make([]byte, 0, 256*n)
	for i := 0; i < n; i++ {
		for c := 0; c < 256; c++ {
			b = append(b, byte(c))
		}
	}
	return b
}

// TestTerminalE2EHappyPath: dashboard ticket -> browser WS -> agent dials
// /ws/terminal -> paired; binary frames relay byte-exact both ways; closing
// the browser closes the agent; the terminal_sessions row is completed with
// the relayed byte counts and open/close are audited.
func TestTerminalE2EHappyPath(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("term-dev")
	opens := h.startManualTerminalAgent("term-dev", token)
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("term-dev") })
	h.enableTerminal("term-dev")
	cookie := h.userCookie("alice")

	browser, agent, sid := h.pairSession(cookie, "term-dev", opens)

	toAgent := [][]byte{[]byte("ls -la\r"), allBytes(4), {0x00, 0xff, 0x1b, '[', 'A'}}
	toBrowser := [][]byte{[]byte("\x1b[1;32m$ \x1b[0m"), allBytes(8), {0xc3, 0x28, 0x00}}
	var in, out int64
	for i := range toAgent {
		relay(t, browser, agent, toAgent[i])
		in += int64(len(toAgent[i]))
		relay(t, agent, browser, toBrowser[i])
		out += int64(len(toBrowser[i]))
	}

	if err := browser.Close(websocket.StatusNormalClosure, "tab closed"); err != nil {
		t.Fatalf("browser close: %v", err)
	}
	expectClose(t, agent, websocket.StatusNormalClosure, "session ended")

	var row terminalRow
	waitFor(t, 3*time.Second, func() bool {
		row, _ = h.terminalSession(sid)
		return row.Ended
	})
	if row.User != "alice" {
		t.Fatalf("session user = %q, want alice", row.User)
	}
	if row.BytesIn != in || row.BytesOut != out || in == 0 || out == 0 {
		t.Fatalf("bytes in/out = %d/%d, want %d/%d", row.BytesIn, row.BytesOut, in, out)
	}
	waitFor(t, 3*time.Second, func() bool { return h.auditCount("alice", "terminal_close", "ok", sid) == 1 })
	if n := h.auditCount("alice", "terminal_open", "ok", sid); n != 1 {
		t.Fatalf("terminal_open ok audits for %s = %d, want 1", sid, n)
	}
	if n := h.termBroker.ActiveCount("term-dev"); n != 0 {
		t.Fatalf("active sessions after close = %d, want 0", n)
	}
}

// TestTerminalAgentCloseEndsBrowser: the agent side closing ends the session
// for the browser and completes the row.
func TestTerminalAgentCloseEndsBrowser(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("term-dev")
	opens := h.startManualTerminalAgent("term-dev", token)
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("term-dev") })
	h.enableTerminal("term-dev")
	cookie := h.loginCookie()

	browser, agent, sid := h.pairSession(cookie, "term-dev", opens)
	relay(t, browser, agent, []byte("exit\r"))
	relay(t, agent, browser, []byte("logout\r\n"))

	if err := agent.Close(websocket.StatusNormalClosure, "shell exited"); err != nil {
		t.Fatalf("agent close: %v", err)
	}
	expectClose(t, browser, websocket.StatusNormalClosure, "session ended")

	var row terminalRow
	waitFor(t, 3*time.Second, func() bool {
		row, _ = h.terminalSession(sid)
		return row.Ended
	})
	if row.BytesIn != 5 || row.BytesOut != 8 {
		t.Fatalf("bytes in/out = %d/%d, want 5/8", row.BytesIn, row.BytesOut)
	}
	waitFor(t, 3*time.Second, func() bool { return h.auditCount("admin", "terminal_close", "ok", sid) == 1 })
	if n := h.auditCount("admin", "terminal_open", "ok", sid); n != 1 {
		t.Fatalf("terminal_open ok audits = %d, want 1", n)
	}
}

// TestTerminalTicketSingleUse: a second use of a browser ticket or an agent
// ticket is rejected, and does not disturb the live session.
func TestTerminalTicketSingleUse(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("term-dev")
	opens := h.startManualTerminalAgent("term-dev", token)
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("term-dev") })
	h.enableTerminal("term-dev")
	cookie := h.loginCookie()

	code, ticket := h.requestTicket(cookie, "term-dev")
	if code != http.StatusOK {
		t.Fatalf("ticket: %d", code)
	}
	req := recvOpen(t, opens)
	browser := h.dialBrowser(ticket)
	agent := h.dialAgentTerminal(req)

	expectClose(t, h.dialBrowser(ticket), websocket.StatusPolicyViolation, "invalid or expired ticket")
	expectClose(t, h.dialAgentTerminal(req), websocket.StatusPolicyViolation, "invalid or expired agent ticket")

	// The original pair is unaffected by the rejected replays.
	relay(t, browser, agent, []byte("still here"))
	relay(t, agent, browser, []byte("yes"))
	if n := h.termBroker.ActiveCount("term-dev"); n != 1 {
		t.Fatalf("active sessions = %d, want 1", n)
	}
}

// TestTerminalTicketExpiry: tickets presented after TicketTTL are rejected on
// the browser socket and on the agent socket, whichever side arrives first,
// and the expiry voids the other side's ticket too.
func TestTerminalTicketExpiry(t *testing.T) {
	// Long reap interval so expiry is detected at consume time, not by the reaper.
	h := newHarnessWithTerminal(t, terminal.Options{TicketTTL: 500 * time.Millisecond, ReapInterval: time.Hour})
	token := h.registerDevice("term-dev")
	opens := h.startManualTerminalAgent("term-dev", token)
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("term-dev") })
	h.enableTerminal("term-dev")
	cookie := h.loginCookie()

	// Browser arrives late first.
	code, ticketA := h.requestTicket(cookie, "term-dev")
	if code != http.StatusOK {
		t.Fatalf("ticket A: %d", code)
	}
	reqA := recvOpen(t, opens)
	// Agent arrives late first.
	code, ticketB := h.requestTicket(cookie, "term-dev")
	if code != http.StatusOK {
		t.Fatalf("ticket B: %d", code)
	}
	reqB := recvOpen(t, opens)

	time.Sleep(800 * time.Millisecond)

	expectClose(t, h.dialBrowser(ticketA), websocket.StatusPolicyViolation, "invalid or expired ticket")
	expectClose(t, h.dialAgentTerminal(reqA), websocket.StatusPolicyViolation, "invalid or expired agent ticket")
	expectClose(t, h.dialAgentTerminal(reqB), websocket.StatusPolicyViolation, "invalid or expired agent ticket")
	expectClose(t, h.dialBrowser(ticketB), websocket.StatusPolicyViolation, "invalid or expired ticket")

	for _, sid := range []string{reqA.SessionID, reqB.SessionID} {
		if n := h.auditCount("admin", "terminal_open", "failed", sid+": ticket expired"); n != 1 {
			t.Fatalf("expiry audits for %s = %d, want 1", sid, n)
		}
		if _, ok := h.terminalSession(sid); ok {
			t.Fatalf("expired session %s has a terminal_sessions row", sid)
		}
	}
	if n := h.termBroker.ActiveCount("term-dev"); n != 0 {
		t.Fatalf("active sessions after expiry = %d, want 0", n)
	}
}

// TestTerminalPairingTimeout: a side that connects but whose peer never
// arrives is closed with 1013 "pairing timeout", and the session's tickets
// are voided. Covered for a waiting browser and for a waiting agent.
func TestTerminalPairingTimeout(t *testing.T) {
	h := newHarnessWithTerminal(t, terminal.Options{TicketTTL: 500 * time.Millisecond, ReapInterval: time.Hour})
	token := h.registerDevice("term-dev")
	opens := h.startManualTerminalAgent("term-dev", token)
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("term-dev") })
	h.enableTerminal("term-dev")
	cookie := h.loginCookie()

	// Browser waits, agent never dials.
	code, ticket := h.requestTicket(cookie, "term-dev")
	if code != http.StatusOK {
		t.Fatalf("ticket: %d", code)
	}
	req := recvOpen(t, opens)
	start := time.Now()
	browser := h.dialBrowser(ticket)
	expectClose(t, browser, websocket.StatusTryAgainLater, "pairing timeout")
	if el := time.Since(start); el < 300*time.Millisecond {
		t.Fatalf("browser closed after %v, before the pairing timeout", el)
	}
	expectClose(t, h.dialAgentTerminal(req), websocket.StatusPolicyViolation, "invalid or expired agent ticket")
	expectClose(t, h.dialBrowser(ticket), websocket.StatusPolicyViolation, "invalid or expired ticket")
	if n := h.auditCount("admin", "terminal_open", "failed", req.SessionID+": pairing timeout"); n != 1 {
		t.Fatalf("pairing timeout audits = %d, want 1", n)
	}
	if _, ok := h.terminalSession(req.SessionID); ok {
		t.Fatal("unpaired session has a terminal_sessions row")
	}

	// Agent waits, browser never connects.
	code, ticket = h.requestTicket(cookie, "term-dev")
	if code != http.StatusOK {
		t.Fatalf("ticket 2: %d", code)
	}
	req = recvOpen(t, opens)
	expectClose(t, h.dialAgentTerminal(req), websocket.StatusTryAgainLater, "pairing timeout")
	expectClose(t, h.dialBrowser(ticket), websocket.StatusPolicyViolation, "invalid or expired ticket")
	if n := h.auditCount("admin", "terminal_open", "failed", req.SessionID+": pairing timeout"); n != 1 {
		t.Fatalf("pairing timeout audits (agent side) = %d, want 1", n)
	}
	if n := h.termBroker.ActiveCount("term-dev"); n != 0 {
		t.Fatalf("active sessions = %d, want 0", n)
	}
}

// TestTerminalConcurrentSessionLimit: two fully paired sessions relay
// independently, a third ticket is refused with 409 session_limit, and ending
// one session frees its slot.
func TestTerminalConcurrentSessionLimit(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("term-dev")
	opens := h.startManualTerminalAgent("term-dev", token)
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("term-dev") })
	h.enableTerminal("term-dev")
	cookie := h.loginCookie()

	b1, a1, _ := h.pairSession(cookie, "term-dev", opens)
	b2, a2, _ := h.pairSession(cookie, "term-dev", opens)
	relay(t, b1, a1, []byte("one"))
	relay(t, b2, a2, []byte("two"))
	relay(t, a2, b2, []byte("TWO"))
	relay(t, a1, b1, []byte("ONE"))

	body, _ := json.Marshal(map[string]int{"cols": 80, "rows": 24})
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/devices/term-dev/terminal", bytes.NewReader(body))
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Error string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || out.Error != "session_limit" {
		t.Fatalf("third session: %d %q, want 409 session_limit", resp.StatusCode, out.Error)
	}
	select {
	case extra := <-opens:
		t.Fatalf("refused session still sent terminal_open %s to the agent", extra.SessionID)
	default:
	}

	b1.Close(websocket.StatusNormalClosure, "")
	waitFor(t, 3*time.Second, func() bool { return h.termBroker.ActiveCount("term-dev") == 1 })
	if code, _ := h.requestTicket(cookie, "term-dev"); code != http.StatusOK {
		t.Fatalf("ticket after freeing a slot: %d, want 200", code)
	}
}

// TestTerminalOpenRefusedWithoutCapability: a real agent without the
// terminal capability is refused by the API (403 terminal_not_capable), and a
// terminal_open forced straight through the hub is refused by the agent
// itself, voiding both tickets without opening a session.
func TestTerminalOpenRefusedWithoutCapability(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("term-dev")
	h.startAgent(h.agentConfig("term-dev", token), nil) // no CapTerminal
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("term-dev") })
	h.enableTerminal("term-dev")
	cookie := h.loginCookie()

	body, _ := json.Marshal(map[string]int{"cols": 80, "rows": 24})
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/devices/term-dev/terminal", bytes.NewReader(body))
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Error string `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || out.Error != "terminal_not_capable" {
		t.Fatalf("API: %d %q, want 403 terminal_not_capable", resp.StatusCode, out.Error)
	}

	// Force terminal_open past the API gate.
	d, err := h.st.GetDevice(context.Background(), "term-dev")
	if err != nil {
		t.Fatal(err)
	}
	tk, err := h.termBroker.CreateTicket("term-dev", d.ID, "admin", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := proto.New(proto.TypeTerminalOpen, proto.TerminalOpen{
		SessionID: tk.SessionID, AgentTicket: tk.AgentTicket, Cols: 80, Rows: 24,
	})
	if err := h.hub.Session("term-dev").Send(env); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return h.termBroker.ActiveCount("term-dev") == 0 })
	if n := h.auditCount("admin", "terminal_open", "failed", tk.SessionID+": agent refused"); n != 1 {
		t.Fatalf("refusal audits = %d, want 1", n)
	}
	expectClose(t, h.dialBrowser(tk.BrowserTicket), websocket.StatusPolicyViolation, "invalid or expired ticket")
	expectClose(t, h.dialAgentTerminal(proto.TerminalOpen{SessionID: tk.SessionID, AgentTicket: tk.AgentTicket}),
		websocket.StatusPolicyViolation, "invalid or expired agent ticket")
	if _, ok := h.terminalSession(tk.SessionID); ok {
		t.Fatal("refused session has a terminal_sessions row")
	}
}

// TestTerminalToggleAudited checks that flipping terminal_enabled through the
// dashboard API writes a dedicated audit row attributed to the logged-in user,
// and that a settings save which leaves the flag unchanged does not.
func TestTerminalToggleAudited(t *testing.T) {
	h := newHarness(t)
	h.registerDevice("toggle-dev")
	cookie := h.userCookie("alice")

	patch := func(enabled bool) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"terminal_enabled": enabled})
		req, _ := http.NewRequest(http.MethodPatch, h.srv.URL+"/api/devices/toggle-dev", bytes.NewReader(body))
		req.AddCookie(cookie)
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PATCH terminal_enabled=%v: status %d", enabled, resp.StatusCode)
		}
	}

	patch(true)
	patch(true) // unchanged: no second terminal_enable row
	patch(false)

	if n := h.auditCount("alice", "terminal_enable", "ok", ""); n != 1 {
		t.Fatalf("terminal_enable rows by alice = %d, want 1", n)
	}
	if n := h.auditCount("alice", "terminal_disable", "ok", ""); n != 1 {
		t.Fatalf("terminal_disable rows by alice = %d, want 1", n)
	}
	if n := h.auditCount("alice", "device_update", "ok", ""); n != 3 {
		t.Fatalf("device_update rows by alice = %d, want 3", n)
	}
	if n := h.auditCount("admin", "device_update", "ok", ""); n != 0 {
		t.Fatalf("device_update rows attributed to hard-coded admin = %d, want 0", n)
	}
}
