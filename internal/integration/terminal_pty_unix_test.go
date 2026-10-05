//go:build !windows

package integration

import (
	"context"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

// TestTerminalRealAgentPTY runs the real agent end to end through the real
// broker: the shell spawns on a PTY, bytes relay both ways, and closing the
// browser socket kills the shell and its background job within 5s.
func TestTerminalRealAgentPTY(t *testing.T) {
	if f, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0); err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	} else {
		f.Close()
	}
	t.Setenv("SHELL", "/bin/sh")
	h := newHarness(t)
	token := h.registerDevice("term-dev")
	cfg := h.agentConfig("term-dev", token)
	cfg.Capabilities = append(cfg.Capabilities, proto.CapTerminal)
	h.startAgent(cfg, nil)
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("term-dev") })
	if err := h.st.UpdateDeviceSettings(context.Background(), "term-dev", "", false, store.Schedule{}, true); err != nil {
		t.Fatal(err)
	}

	code, ticket := h.requestTicket(h.loginCookie(), "term-dev")
	if code != http.StatusOK {
		t.Fatalf("ticket: %d", code)
	}
	browser := h.dialBrowser(ticket)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := browser.Write(ctx, websocket.MessageBinary, []byte("set -m; sleep 300 & echo BG_$!; echo SH_$$; cat\n")); err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`BG_(\d+)[\s\S]*SH_(\d+)`)
	var seen []byte
	var m [][]byte
	for m == nil {
		typ, data, err := browser.Read(ctx)
		if err != nil {
			t.Fatalf("browser read: %v (seen %q)", err, seen)
		}
		if typ == websocket.MessageBinary {
			seen = append(seen, data...)
			m = re.FindSubmatch(seen)
		}
	}
	bg, _ := strconv.Atoi(string(m[1]))
	shell, _ := strconv.Atoi(string(m[2]))

	browser.Close(websocket.StatusNormalClosure, "tab closed")
	for _, pid := range []int{shell, bg} {
		waitFor(t, 5*time.Second, func() bool { return !pidAlive(pid) })
	}
}

// pidAlive treats a zombie as dead: orphans wait for init to reap them.
func pidAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	re := regexp.MustCompile(`\) (\S)`)
	st := re.FindSubmatch(b)
	return st == nil || string(st[1]) != "Z"
}
