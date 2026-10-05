package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jhyoong/KumaBoard/server/terminal"
)

// Server-enforced session ends: disabling the terminal, logging out and
// revoking the device each end live sessions and void pending tickets.

// apiCall sends a dashboard API request with cookie and returns the status.
func (h *harness) apiCall(cookie *http.Cookie, method, path string, body any) int {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, &buf)
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// waitSessionEnded waits for sid's terminal_sessions row to be completed.
func (h *harness) waitSessionEnded(sid string) {
	h.t.Helper()
	waitFor(h.t, 3*time.Second, func() bool {
		row, ok := h.terminalSession(sid)
		return ok && row.Ended
	})
}

func TestTerminalDisableEndsSessions(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("term-dev")
	opens := h.startManualTerminalAgent("term-dev", token)
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("term-dev") })
	h.enableTerminal("term-dev")
	cookie := h.userCookie("alice")

	browser, agent, sid := h.pairSession(cookie, "term-dev", opens)
	relay(t, browser, agent, []byte("top\r"))

	// A second ticket is issued but neither side has connected yet.
	code, ticket := h.requestTicket(cookie, "term-dev")
	if code != http.StatusOK {
		t.Fatalf("pending ticket: %d", code)
	}
	pending := recvOpen(t, opens)

	if code := h.apiCall(cookie, http.MethodPatch, "/api/devices/term-dev", map[string]any{"terminal_enabled": false}); code != http.StatusOK {
		t.Fatalf("disable: %d", code)
	}

	expectClose(t, browser, websocket.StatusPolicyViolation, "terminal disabled")
	expectClose(t, agent, websocket.StatusPolicyViolation, "terminal disabled")
	h.waitSessionEnded(sid)
	waitFor(t, 3*time.Second, func() bool {
		return h.auditCount("alice", "terminal_close", "ok", sid+": terminal disabled") == 1
	})
	if n := h.auditCount("alice", "terminal_open", "failed", sid); n != 0 {
		t.Fatalf("live session also recorded as failed (%d rows)", n)
	}

	// The pending ticket is void on both sides.
	expectClose(t, h.dialBrowser(ticket), websocket.StatusPolicyViolation, "invalid or expired ticket")
	expectClose(t, h.dialAgentTerminal(pending), websocket.StatusPolicyViolation, "invalid or expired agent ticket")
	if n := h.auditCount("alice", "terminal_open", "failed", pending.SessionID+": terminal disabled"); n != 1 {
		t.Fatalf("pending ticket failed-open audits = %d, want 1", n)
	}
	if n := h.termBroker.ActiveCount("term-dev"); n != 0 {
		t.Fatalf("active sessions = %d, want 0", n)
	}
	// And new tickets are refused while disabled.
	if code, _ := h.requestTicket(cookie, "term-dev"); code != http.StatusForbidden {
		t.Fatalf("ticket after disable: %d, want 403", code)
	}
}

func TestTerminalLogoutEndsUserSessions(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("term-dev")
	opens := h.startManualTerminalAgent("term-dev", token)
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("term-dev") })
	h.enableTerminal("term-dev")
	alice := h.userCookie("alice")
	bob := h.userCookie("bob")

	aBrowser, aAgent, aSid := h.pairSession(alice, "term-dev", opens)
	bBrowser, bAgent, bSid := h.pairSession(bob, "term-dev", opens)

	if code := h.apiCall(alice, http.MethodPost, "/api/logout", nil); code != http.StatusNoContent {
		t.Fatalf("logout: %d", code)
	}

	expectClose(t, aBrowser, websocket.StatusPolicyViolation, "logged out")
	expectClose(t, aAgent, websocket.StatusPolicyViolation, "logged out")
	h.waitSessionEnded(aSid)
	waitFor(t, 3*time.Second, func() bool {
		return h.auditCount("alice", "terminal_close", "ok", aSid+": logged out") == 1
	})

	// Bob's session is unaffected.
	relay(t, bBrowser, bAgent, []byte("still here\r"))
	relay(t, bAgent, bBrowser, []byte("ok\r\n"))
	if row, _ := h.terminalSession(bSid); row.Ended {
		t.Fatal("bob's session ended by alice's logout")
	}
}

func TestTerminalRevokeEndsSessions(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("term-dev")
	opens := h.startManualTerminalAgent("term-dev", token)
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("term-dev") })
	h.enableTerminal("term-dev")
	cookie := h.userCookie("alice")

	browser, agent, sid := h.pairSession(cookie, "term-dev", opens)
	relay(t, browser, agent, []byte("id\r"))

	if code := h.apiCall(cookie, http.MethodPost, "/api/devices/term-dev/revoke", nil); code != http.StatusOK {
		t.Fatalf("revoke: %d", code)
	}

	// The terminal socket is ticket-authenticated and independent of the
	// control connection; the server must close it itself.
	expectClose(t, browser, websocket.StatusPolicyViolation, "device revoked")
	expectClose(t, agent, websocket.StatusPolicyViolation, "device revoked")
	h.waitSessionEnded(sid)
	waitFor(t, 3*time.Second, func() bool {
		return h.auditCount("alice", "terminal_close", "ok", sid+": device revoked") == 1
	})
	if n := h.termBroker.ActiveCount("term-dev"); n != 0 {
		t.Fatalf("active sessions = %d, want 0", n)
	}
}

// TestTerminalPairingDeadlineFromIssue: a browser that attaches late in the
// ticket window is closed when the window (measured from issue) ends, not a
// full TTL after it attached.
func TestTerminalPairingDeadlineFromIssue(t *testing.T) {
	const ttl = time.Second
	h := newHarnessWithTerminal(t, terminal.Options{TicketTTL: ttl, ReapInterval: time.Hour})
	token := h.registerDevice("term-dev")
	opens := h.startManualTerminalAgent("term-dev", token)
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("term-dev") })
	h.enableTerminal("term-dev")
	cookie := h.loginCookie()

	issued := time.Now()
	code, ticket := h.requestTicket(cookie, "term-dev")
	if code != http.StatusOK {
		t.Fatalf("ticket: %d", code)
	}
	req := recvOpen(t, opens)

	time.Sleep(ttl * 4 / 5)
	browser := h.dialBrowser(ticket)
	expectClose(t, browser, websocket.StatusTryAgainLater, "pairing timeout")
	// Measured from attach this would end near 1.8*ttl.
	if el := time.Since(issued); el > ttl*7/5 {
		t.Fatalf("pairing ended %v after issue, want at most about %v", el, ttl)
	}
	expectClose(t, h.dialAgentTerminal(req), websocket.StatusPolicyViolation, "invalid or expired agent ticket")
}

// TestTerminalPreHelloLimits: over real TLS, a /ws/terminal socket that never
// sends its hello is closed after the hello timeout, at most 16 such sockets
// may be pending at once (the 17th upgrade gets 503), and the slots free up
// once the silent sockets are closed.
func TestTerminalPreHelloLimits(t *testing.T) {
	const helloTimeout = 700 * time.Millisecond
	h := newHarnessWithTerminal(t, terminal.Options{HelloTimeout: helloTimeout})

	dial := func() (*websocket.Conn, int, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		conn, resp, err := websocket.Dial(ctx, "wss://"+h.addr+"/ws/terminal", &websocket.DialOptions{HTTPClient: h.srv.Client()})
		if conn != nil {
			t.Cleanup(func() { conn.CloseNow() })
		}
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		return conn, code, err
	}

	const maxPending = 16
	start := time.Now()
	var silent []*websocket.Conn
	for i := 0; i < maxPending; i++ {
		conn, _, err := dial()
		if err != nil {
			t.Fatalf("socket %d: %v", i, err)
		}
		silent = append(silent, conn)
	}
	if _, code, err := dial(); err == nil || code != http.StatusServiceUnavailable {
		t.Fatalf("socket over the cap: err=%v status=%d, want 503", err, code)
	}

	for _, conn := range silent {
		expectClose(t, conn, websocket.StatusPolicyViolation, "hello timeout")
	}
	if el := time.Since(start); el < helloTimeout/2 {
		t.Fatalf("silent sockets closed after %v, before the hello timeout", el)
	}

	// With the silent sockets gone, new sockets are accepted again.
	waitFor(t, 3*time.Second, func() bool {
		conn, _, err := dial()
		if err != nil {
			return false
		}
		conn.CloseNow()
		return true
	})
}
