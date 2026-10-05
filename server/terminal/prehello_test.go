package terminal

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// dialSilent opens a terminal socket and sends nothing.
func dialSilent(t *testing.T, url string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, url, nil)
	if conn != nil {
		t.Cleanup(func() { conn.CloseNow() })
	}
	return conn, resp, err
}

func TestHelloTimeoutClosesSilentSocket(t *testing.T) {
	const hello = 150 * time.Millisecond
	b := newTestBroker(t, nil, Options{HelloTimeout: hello})
	url := serveBroker(t, b)

	conn, _, err := dialSilent(t, url)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	expectClose(t, conn, websocket.StatusPolicyViolation, ReasonHelloTimeout)
	if el := time.Since(start); el > hello+time.Second {
		t.Fatalf("closed after %v, want about %v", el, hello)
	}
	waitFor(t, func() bool { return len(b.preHello) == 0 })
}

func TestPendingHelloCap(t *testing.T) {
	b := newTestBroker(t, nil, Options{HelloTimeout: time.Minute})
	url := serveBroker(t, b)

	var silent []*websocket.Conn
	for i := 0; i < maxPendingHellos; i++ {
		conn, _, err := dialSilent(t, url)
		if err != nil {
			t.Fatalf("socket %d: %v", i, err)
		}
		silent = append(silent, conn)
	}
	waitFor(t, func() bool { return len(b.preHello) == maxPendingHellos })

	// One more is refused before the upgrade.
	if _, resp, err := dialSilent(t, url); err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("socket over the cap: err=%v status=%d, want 503", err, code)
	}

	// A socket that leaves frees its slot, and a socket that sends its hello
	// no longer counts against the cap.
	silent[0].CloseNow()
	waitFor(t, func() bool { return len(b.preHello) == maxPendingHellos-1 })
	tk, _ := b.CreateTicket("dev-1", 42, "admin", 80, 24)
	browser := dialHello(t, url, browserHello(tk))
	waitFor(t, func() bool { return len(b.preHello) == maxPendingHellos-1 })
	agent := dialHello(t, url, agentHello(tk))
	waitFor(t, func() bool { return len(b.preHello) == maxPendingHellos-1 })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := browser.Write(ctx, websocket.MessageBinary, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if _, data, err := agent.Read(ctx); err != nil || string(data) != "hi" {
		t.Fatalf("relay under load: %q %v", data, err)
	}
}
