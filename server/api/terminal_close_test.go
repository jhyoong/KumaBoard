package api

import (
	"context"
	"testing"

	"github.com/jhyoong/KumaBoard/server/store"
)

func TestDisableTerminalVoidsTickets(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	ctx := context.Background()
	d, _, err := e.st.CreateDevice(ctx, "dev-1", "", false, store.Schedule{})
	if err != nil {
		t.Fatal(err)
	}
	patch := func(enabled bool) {
		t.Helper()
		resp := e.do(t, "PATCH", "/api/devices/dev-1", map[string]any{"terminal_enabled": enabled})
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("PATCH terminal_enabled=%v: %d", enabled, resp.StatusCode)
		}
	}

	patch(true)
	tk, err := e.term.CreateTicket("dev-1", d.ID, "admin", 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	// A save that leaves the flag on keeps the ticket.
	patch(true)
	if n := e.term.ActiveCount("dev-1"); n != 1 {
		t.Fatalf("active after unchanged save = %d, want 1", n)
	}

	patch(false)
	if n := e.term.ActiveCount("dev-1"); n != 0 {
		t.Fatalf("active after disable = %d, want 0", n)
	}
	if _, ok := e.term.ConsumeBrowserTicket(tk.BrowserTicket); ok {
		t.Fatal("ticket valid after disable")
	}
}

func TestRevokeVoidsTickets(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	d, _, _ := e.st.CreateDevice(context.Background(), "dev-1", "", false, store.Schedule{})
	tk, _ := e.term.CreateTicket("dev-1", d.ID, "admin", 80, 24)

	resp := e.do(t, "POST", "/api/devices/dev-1/revoke", nil)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	if _, ok := e.term.ConsumeAgentTicket(tk.SessionID, tk.AgentTicket); ok {
		t.Fatal("agent ticket valid after revoke")
	}
	if n := e.term.ActiveCount("dev-1"); n != 0 {
		t.Fatalf("active after revoke = %d, want 0", n)
	}
}

func TestLogoutVoidsUserTickets(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	mine, _ := e.term.CreateTicket("dev-1", 1, "admin", 80, 24)
	theirs, _ := e.term.CreateTicket("dev-1", 1, "bob", 80, 24)

	resp := e.do(t, "POST", "/api/logout", nil)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if _, ok := e.term.ConsumeBrowserTicket(mine.BrowserTicket); ok {
		t.Fatal("logged-out user's ticket still valid")
	}
	if _, ok := e.term.ConsumeBrowserTicket(theirs.BrowserTicket); !ok {
		t.Fatal("another user's ticket voided by logout")
	}
}
