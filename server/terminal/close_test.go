package terminal

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jhyoong/KumaBoard/server/store"
)

func auditDetails(t *testing.T, st *store.Store, action, result string) []string {
	t.Helper()
	entries, err := st.ListAudit(context.Background(), 500, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.Action == action && e.Result == result {
			out = append(out, e.Detail)
		}
	}
	return out
}

func TestCloseDeviceEndsLiveAndPending(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	d1, _, _ := st.CreateDevice(ctx, "dev-1", "", false, store.Schedule{})
	d2, _, _ := st.CreateDevice(ctx, "dev-2", "", false, store.Schedule{})
	b := newTestBroker(t, st, Options{})
	url := serveBroker(t, b)

	live, _ := b.CreateTicket("dev-1", d1.ID, "admin", 80, 24)
	browser, agent := pairSession(t, b, url, live)
	waitFor(t, func() bool { n, _ := st.CountActiveTerminalSessions(ctx, d1.ID); return n == 1 })
	pending, _ := b.CreateTicket("dev-1", d1.ID, "admin", 80, 24)
	other, _ := b.CreateTicket("dev-2", d2.ID, "admin", 80, 24)

	if n := b.CloseDevice("dev-1", ReasonTerminalDisabled); n != 2 {
		t.Fatalf("CloseDevice ended %d sessions, want 2", n)
	}
	expectClose(t, browser, websocket.StatusPolicyViolation, ReasonTerminalDisabled)
	expectClose(t, agent, websocket.StatusPolicyViolation, ReasonTerminalDisabled)

	// The pending ticket is void on both sides.
	if _, ok := b.ConsumeBrowserTicket(pending.BrowserTicket); ok {
		t.Fatal("pending browser ticket valid after CloseDevice")
	}
	if _, ok := b.ConsumeAgentTicket(pending.SessionID, pending.AgentTicket); ok {
		t.Fatal("pending agent ticket valid after CloseDevice")
	}
	if n := b.ActiveCount("dev-1"); n != 0 {
		t.Fatalf("dev-1 active = %d, want 0", n)
	}
	// Other devices are untouched.
	if n := b.ActiveCount("dev-2"); n != 1 {
		t.Fatalf("dev-2 active = %d, want 1", n)
	}
	if _, ok := b.ConsumeBrowserTicket(other.BrowserTicket); !ok {
		t.Fatal("other device's ticket voided")
	}

	// The live session is recorded as closed (not failed) with the reason;
	// the pending one as a failed open.
	waitFor(t, func() bool { return countAudit(t, st, "terminal_close", "ok") == 1 })
	if got := auditDetails(t, st, "terminal_close", "ok"); got[0] != live.SessionID+": "+ReasonTerminalDisabled {
		t.Fatalf("close detail = %q", got[0])
	}
	failed := auditDetails(t, st, "terminal_open", "failed")
	if len(failed) != 1 || failed[0] != pending.SessionID+": "+ReasonTerminalDisabled {
		t.Fatalf("failed opens = %q, want only the pending ticket", failed)
	}
	if n, _ := st.CountActiveTerminalSessions(ctx, d1.ID); n != 0 {
		t.Fatalf("terminal_sessions row still open: %d", n)
	}
}

func TestCloseUserEndsOnlyThatUser(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	d, _, _ := st.CreateDevice(ctx, "dev-1", "", false, store.Schedule{})
	b := newTestBroker(t, st, Options{})
	url := serveBroker(t, b)

	alice, _ := b.CreateTicket("dev-1", d.ID, "alice", 80, 24)
	aBrowser, aAgent := pairSession(t, b, url, alice)
	bob, _ := b.CreateTicket("dev-1", d.ID, "bob", 80, 24)
	bBrowser, bAgent := pairSession(t, b, url, bob)
	alicePending, _ := b.CreateTicket("dev-2", 99, "alice", 80, 24)

	if n := b.CloseUser("alice", ReasonLoggedOut); n != 2 {
		t.Fatalf("CloseUser ended %d sessions, want 2", n)
	}
	expectClose(t, aBrowser, websocket.StatusPolicyViolation, ReasonLoggedOut)
	expectClose(t, aAgent, websocket.StatusPolicyViolation, ReasonLoggedOut)
	if _, ok := b.ConsumeBrowserTicket(alicePending.BrowserTicket); ok {
		t.Fatal("alice's pending ticket valid after CloseUser")
	}

	// Bob's session still relays.
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := bBrowser.Write(wctx, websocket.MessageBinary, []byte("still here")); err != nil {
		t.Fatal(err)
	}
	if _, data, err := bAgent.Read(wctx); err != nil || string(data) != "still here" {
		t.Fatalf("bob's session broken: %q %v", data, err)
	}
	if n := b.ActiveCount("dev-1"); n != 1 {
		t.Fatalf("dev-1 active = %d, want 1 (bob)", n)
	}
}

func TestCloseDeviceIdempotent(t *testing.T) {
	b := newTestBroker(t, nil, Options{})
	b.CreateTicket("dev-1", 1, "admin", 80, 24)
	if n := b.CloseDevice("dev-1", ReasonDeviceRevoked); n != 1 {
		t.Fatalf("first close = %d, want 1", n)
	}
	if n := b.CloseDevice("dev-1", ReasonDeviceRevoked); n != 0 {
		t.Fatalf("second close = %d, want 0", n)
	}
}

func TestRefuseSessionIgnoresPaired(t *testing.T) {
	st := testStore(t)
	d, _, _ := st.CreateDevice(context.Background(), "dev-1", "", false, store.Schedule{})
	b := newTestBroker(t, st, Options{})
	url := serveBroker(t, b)
	tk, _ := b.CreateTicket("dev-1", d.ID, "admin", 80, 24)
	browser, agent := pairSession(t, b, url, tk)

	if b.RefuseSession("dev-1", tk.SessionID) {
		t.Fatal("paired session refused")
	}
	b.CancelTicket(tk.SessionID) // also a no-op once paired
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := browser.Write(ctx, websocket.MessageBinary, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := agent.Read(ctx); err != nil {
		t.Fatalf("session broken by late refusal: %v", err)
	}
	if n := countAudit(t, st, "terminal_open", "failed"); n != 0 {
		t.Fatalf("failed-open audits = %d, want 0", n)
	}
}

// TestRefuseRacingPairRecordsOneOutcome races RefuseSession against pairing
// and checks every session ends with exactly one outcome: a failed open, or
// an ok open followed by an ok close.
func TestRefuseRacingPairRecordsOneOutcome(t *testing.T) {
	st := testStore(t)
	d, _, _ := st.CreateDevice(context.Background(), "dev-1", "", false, store.Schedule{})
	b := newTestBroker(t, st, Options{})
	url := serveBroker(t, b)

	const rounds = 40
	var ids []string
	for i := 0; i < rounds; i++ {
		tk, err := b.CreateTicket("dev-1", d.ID, "admin", 80, 24)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, tk.SessionID)
		browser := dialHello(t, url, browserHello(tk))
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			dialHello(t, url, agentHello(tk))
		}()
		// Refuse at varying points around the moment of pairing.
		time.Sleep(time.Duration(i%8) * 250 * time.Microsecond)
		b.RefuseSession("dev-1", tk.SessionID)
		wg.Wait()
		browser.CloseNow()
		waitFor(t, func() bool { return b.ActiveCount("dev-1") == 0 })
	}

	waitFor(t, func() bool {
		return countAudit(t, st, "terminal_open", "failed")+countAudit(t, st, "terminal_close", "ok") == rounds
	})
	entries, _ := st.ListAudit(context.Background(), 1000, 0)
	for _, id := range ids {
		outcome := map[string]int{}
		for _, e := range entries {
			if strings.HasPrefix(e.Detail, id) {
				outcome[e.Action+"/"+e.Result]++
			}
		}
		refused := outcome["terminal_open/failed"] == 1 && len(outcome) == 1
		opened := outcome["terminal_open/ok"] == 1 && outcome["terminal_close/ok"] == 1 && len(outcome) == 2
		if !refused && !opened {
			t.Fatalf("session %s outcomes = %v, want exactly one", id, outcome)
		}
	}
}
