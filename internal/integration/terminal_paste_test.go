//go:build !windows

package integration

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

// TestTerminalLargeFrameReachesShell sends one browser frame larger than
// coder/websocket's 32 KiB default read limit but within the server's 64 KiB
// limit. It must reach the agent's shell intact without closing the session.
func TestTerminalLargeFrameReachesShell(t *testing.T) {
	if f, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0); err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	} else {
		f.Close()
	}
	t.Setenv("SHELL", "/bin/sh")
	h := newHarness(t)
	token := h.registerDevice("paste-dev")
	cfg := h.agentConfig("paste-dev", token)
	cfg.Capabilities = append(cfg.Capabilities, proto.CapTerminal)
	h.startAgent(cfg, nil)
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("paste-dev") })
	if err := h.st.UpdateDeviceSettings(context.Background(), "paste-dev", "", false, store.Schedule{}, true); err != nil {
		t.Fatal(err)
	}

	code, ticket := h.requestTicket(h.loginCookie(), "paste-dev")
	if code != http.StatusOK {
		t.Fatalf("ticket: %d", code)
	}
	browser := h.dialBrowser(ticket)
	defer browser.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const size = 48 << 10
	if size <= 32<<10 || size > proto.TerminalMaxFrame {
		t.Fatalf("payload size %d must be in (32 KiB, %d]", size, proto.TerminalMaxFrame)
	}

	var seen []byte
	readUntil := func(marker string) {
		t.Helper()
		for !bytes.Contains(seen, []byte(marker)) {
			typ, data, err := browser.Read(ctx)
			if err != nil {
				t.Fatalf("browser read waiting for %q: %v (seen %q)", marker, err, tailBytes(seen))
			}
			if typ == websocket.MessageBinary {
				seen = append(seen, data...)
			}
		}
	}

	// Non-canonical, no echo: the shell reads the paste byte for byte, free of
	// the line discipline's line length cap. The quoted markers keep the
	// echoed command text from matching.
	setup := "stty -icanon -echo; echo RE''ADY; head -c " + strconv.Itoa(size) + " | wc -c; echo DO''NE\n"
	if err := browser.Write(ctx, websocket.MessageBinary, []byte(setup)); err != nil {
		t.Fatal(err)
	}
	readUntil("READY")
	seen = nil

	if err := browser.Write(ctx, websocket.MessageBinary, bytes.Repeat([]byte("a"), size)); err != nil {
		t.Fatal(err)
	}
	readUntil("DONE")
	if !bytes.Contains(seen, []byte(strconv.Itoa(size))) {
		t.Fatalf("shell did not receive %d bytes; output %q", size, tailBytes(seen))
	}

	// The session is still live after the large frame.
	if err := browser.Write(ctx, websocket.MessageBinary, []byte("echo AL''IVE\n")); err != nil {
		t.Fatal(err)
	}
	readUntil("ALIVE")
}

func tailBytes(b []byte) []byte {
	if len(b) > 512 {
		return b[len(b)-512:]
	}
	return b
}
