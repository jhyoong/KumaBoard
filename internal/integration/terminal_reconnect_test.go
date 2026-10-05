//go:build !windows

package integration

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jhyoong/KumaBoard/proto"
)

// openRealShell starts a real terminal-capable agent, opens a PTY session
// through the broker, and returns the browser socket and the shell's pid.
func openRealShell(t *testing.T, h *harness, device string) (*websocket.Conn, int) {
	t.Helper()
	if f, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0); err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	} else {
		f.Close()
	}
	t.Setenv("SHELL", "/bin/sh")
	token := h.registerDevice(device)
	cfg := h.agentConfig(device, token)
	cfg.Capabilities = append(cfg.Capabilities, proto.CapTerminal)
	ag := h.startAgent(cfg, nil)
	t.Cleanup(func() { ag.app.Close(5 * time.Second) })
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected(device) })
	h.enableTerminal(device)

	code, ticket := h.requestTicket(h.loginCookie(), device)
	if code != http.StatusOK {
		t.Fatalf("ticket: %d", code)
	}
	browser := h.dialBrowser(ticket)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := browser.Write(ctx, websocket.MessageBinary, []byte("echo SH_$$; cat\n")); err != nil {
		t.Fatal(err)
	}
	m := readUntil(t, ctx, browser, regexp.MustCompile(`SH_(\d+)`))
	pid, _ := strconv.Atoi(string(m[1]))
	return browser, pid
}

func readUntil(t *testing.T, ctx context.Context, conn *websocket.Conn, re *regexp.Regexp) [][]byte {
	t.Helper()
	var seen []byte
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("browser read: %v (seen %q)", err, seen)
		}
		if typ == websocket.MessageBinary {
			seen = append(seen, data...)
			if m := re.FindSubmatch(seen); m != nil {
				return m
			}
		}
	}
}

// reconnectControl drops the device's control connection and waits for the
// agent to come back with a new session.
func reconnectControl(t *testing.T, h *harness, device string) {
	t.Helper()
	old := h.hub.Session(device)
	h.hub.CloseDevice(device, "network blip")
	waitFor(t, 5*time.Second, func() bool {
		s := h.hub.Session(device)
		return s != nil && s != old
	})
}

// TestTerminalSurvivesControlReconnect: a control-connection drop and
// reconnect leaves a live shell running and relaying.
func TestTerminalSurvivesControlReconnect(t *testing.T) {
	h := newHarness(t)
	browser, pid := openRealShell(t, h, "term-dev")

	reconnectControl(t, h, "term-dev")
	reconnectControl(t, h, "term-dev")
	if !pidAlive(pid) {
		t.Fatal("shell killed by a control reconnect")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := browser.Write(ctx, websocket.MessageBinary, []byte("still_here\n")); err != nil {
		t.Fatal(err)
	}
	// cat echoes the line back (plus the tty echo).
	m := readUntil(t, ctx, browser, regexp.MustCompile(`still_here`))
	if !bytes.Equal(m[0], []byte("still_here")) {
		t.Fatalf("unexpected match %q", m[0])
	}
}

// TestTerminalKilledWhenReconnectRejected: after the token is revoked, the
// agent's next reconnect is rejected with auth_failed and the agent kills the
// shell itself. The server-side terminal close on revoke lives in the API
// handler, which this test bypasses (store + hub directly), so only the
// agent-side kill is exercised here.
func TestTerminalKilledWhenReconnectRejected(t *testing.T) {
	h := newHarness(t)
	browser, pid := openRealShell(t, h, "term-dev")

	if err := h.st.RevokeToken(context.Background(), "term-dev"); err != nil {
		t.Fatal(err)
	}
	h.hub.CloseDevice("term-dev", "token revoked")
	waitFor(t, 5*time.Second, func() bool { return !pidAlive(pid) })
	if h.hub.Connected("term-dev") {
		t.Fatal("revoked agent reconnected")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := browser.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("browser socket still open after the shell was killed")
			}
			return
		}
	}
}
