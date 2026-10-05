package terminal

import (
	"context"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/server/store"
)

func TestTicketCreateAndConsume(t *testing.T) {
	b := newTestBroker(t, nil, Options{})

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
	// Unknown and agent tickets are not browser tickets.
	if _, ok := b.ConsumeBrowserTicket("nope"); ok {
		t.Fatal("unknown browser ticket accepted")
	}
	if _, ok := b.ConsumeBrowserTicket(tk.AgentTicket); ok {
		t.Fatal("agent ticket accepted as browser ticket")
	}
}

func TestAgentTicketConsume(t *testing.T) {
	b := newTestBroker(t, nil, Options{})

	tk, _ := b.CreateTicket("dev-1", 42, "admin", 80, 24)

	if _, ok := b.ConsumeAgentTicket(tk.SessionID, tk.BrowserTicket); ok {
		t.Fatal("browser ticket accepted as agent ticket")
	}
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
	b := newTestBroker(t, nil, Options{TicketTTL: time.Millisecond, ReapInterval: time.Hour})

	tk, _ := b.CreateTicket("dev-1", 42, "admin", 80, 24)
	time.Sleep(5 * time.Millisecond)

	if _, ok := b.ConsumeBrowserTicket(tk.BrowserTicket); ok {
		t.Fatal("expired browser ticket accepted")
	}
	if _, ok := b.ConsumeAgentTicket(tk.SessionID, tk.AgentTicket); ok {
		t.Fatal("expired agent ticket accepted")
	}
	if n := b.ActiveCount("dev-1"); n != 0 {
		t.Fatalf("expired session still counted: %d", n)
	}
}

func TestSessionLimit(t *testing.T) {
	b := newTestBroker(t, nil, Options{})

	b.CreateTicket("dev-1", 42, "admin", 80, 24)
	b.CreateTicket("dev-1", 42, "admin", 80, 24)

	_, err := b.CreateTicket("dev-1", 42, "admin", 80, 24)
	if err != ErrSessionLimit {
		t.Fatalf("expected ErrSessionLimit, got %v", err)
	}
	// The limit is per device.
	if _, err := b.CreateTicket("dev-2", 43, "admin", 80, 24); err != nil {
		t.Fatalf("other device blocked: %v", err)
	}
}

func TestReaperFreesAbandonedTickets(t *testing.T) {
	st := testStore(t)
	d, _, _ := st.CreateDevice(context.Background(), "dev-1", "", false, store.Schedule{})
	b := newTestBroker(t, st, Options{TicketTTL: 20 * time.Millisecond, ReapInterval: 5 * time.Millisecond})

	for i := 0; i < 2; i++ {
		if _, err := b.CreateTicket("dev-1", d.ID, "admin", 80, 24); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.CreateTicket("dev-1", d.ID, "admin", 80, 24); err != ErrSessionLimit {
		t.Fatalf("want limit before reaping, got %v", err)
	}

	// Nobody ever presents the tickets; the reaper alone must free the slots.
	waitFor(t, func() bool { return b.ActiveCount("dev-1") == 0 })
	if _, err := b.CreateTicket("dev-1", d.ID, "admin", 80, 24); err != nil {
		t.Fatalf("device slot not freed after reaping: %v", err)
	}
	if n := countAudit(t, st, "terminal_open", "failed"); n != 2 {
		t.Fatalf("want 2 failed-open audits, got %d", n)
	}
}

func TestStartupSweepsOrphanRows(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	d, _, _ := st.CreateDevice(ctx, "dev-1", "", false, store.Schedule{})
	// Rows left open by a crashed process.
	st.InsertTerminalSession(ctx, "orphan-1", d.ID, "admin")
	st.InsertTerminalSession(ctx, "orphan-2", d.ID, "admin")

	b := newTestBroker(t, st, Options{})

	if n, _ := st.CountActiveTerminalSessions(ctx, d.ID); n != 0 {
		t.Fatalf("orphan rows still open: %d", n)
	}
	for i := 0; i < 2; i++ {
		if _, err := b.CreateTicket("dev-1", d.ID, "admin", 80, 24); err != nil {
			t.Fatalf("ticket %d blocked by orphans: %v", i, err)
		}
	}
}

func TestRefuseSessionIgnoresOtherDevice(t *testing.T) {
	b := newTestBroker(t, nil, Options{})
	tk, _ := b.CreateTicket("dev-1", 42, "admin", 80, 24)

	if b.RefuseSession("dev-2", tk.SessionID) {
		t.Fatal("another device's refusal failed the session")
	}
	if b.RefuseSession("dev-1", "unknown") {
		t.Fatal("unknown session refused")
	}
	if !b.RefuseSession("dev-1", tk.SessionID) {
		t.Fatal("refusal not applied")
	}
	if _, ok := b.ConsumeAgentTicket(tk.SessionID, tk.AgentTicket); ok {
		t.Fatal("agent ticket valid after refusal")
	}
	if _, ok := b.ConsumeBrowserTicket(tk.BrowserTicket); ok {
		t.Fatal("browser ticket valid after refusal")
	}
	if n := b.ActiveCount("dev-1"); n != 0 {
		t.Fatalf("slot not freed: %d", n)
	}
}
