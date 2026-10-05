package app

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/coder/websocket"

	agentterm "github.com/jhyoong/KumaBoard/agent/terminal"
	"github.com/jhyoong/KumaBoard/agent/transport"
	"github.com/jhyoong/KumaBoard/proto"
)

// TestTerminalRegistry covers the registry without a PTY: release removes an
// entry, and only an auth rejection cancels live sessions.
func TestTerminalRegistry(t *testing.T) {
	a := testApp(t, "wss://127.0.0.1:1/ws", nil, proto.CapTerminal)
	ctx1, release1 := a.trackTerminal("s1")
	ctx2, release2 := a.trackTerminal("s2")
	if n := a.liveTerminalCount(); n != 2 {
		t.Fatalf("live %d, want 2", n)
	}

	// A control reconnect is not a lifetime event for terminals.
	a.OnConnected(proto.HelloAck{}, make(fakeSender, 4))
	a.OnDisconnected()
	a.OnConnected(proto.HelloAck{}, make(fakeSender, 4))
	a.OnDisconnected()
	// Non-auth rejections (and plain errors) leave them alone too.
	a.OnRejected(&transport.HandshakeError{Code: proto.ErrProtocolVersionUnsupported})
	a.OnRejected(errors.New("network down"))
	if ctx1.Err() != nil || ctx2.Err() != nil {
		t.Fatal("terminal context cancelled without revocation")
	}

	release1()
	if n := a.liveTerminalCount(); n != 1 {
		t.Fatalf("live %d after release, want 1", n)
	}

	a.OnRejected(&transport.HandshakeError{Code: proto.ErrAuthFailed})
	if ctx2.Err() == nil {
		t.Fatal("auth rejection did not cancel the live terminal")
	}
	release2()
	if n := a.liveTerminalCount(); n != 0 {
		t.Fatalf("live %d after all released, want 0", n)
	}
}

// TestTerminalCloseCancelsSessions checks Close (process shutdown) ends
// sessions and waits for them to release.
func TestTerminalCloseCancelsSessions(t *testing.T) {
	a := testApp(t, "wss://127.0.0.1:1/ws", nil, proto.CapTerminal)
	ctx, release := a.trackTerminal("s1")
	go func() {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		release()
	}()
	start := time.Now()
	a.Close(5 * time.Second)
	if a.liveTerminalCount() != 0 {
		t.Fatal("Close returned before the session released")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("Close waited too long")
	}
}

// holdingTerminalServer accepts /ws/terminal, reads (and discards) frames
// until the agent closes the socket, then signals closed.
func holdingTerminalServer(t *testing.T) (*httptest.Server, chan struct{}, chan struct{}) {
	t.Helper()
	accepted := make(chan struct{}, 1)
	closed := make(chan struct{}, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		accepted <- struct{}{}
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				closed <- struct{}{}
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, accepted, closed
}

// TestTerminalSurvivesReconnectDiesOnAuthReject runs a real PTY session:
// control reconnects leave it up, an auth-rejected reconnect kills it.
func TestTerminalSurvivesReconnectDiesOnAuthReject(t *testing.T) {
	if !agentterm.Supported {
		t.Skip("terminal unsupported on this platform")
	}
	if f, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0); err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	} else {
		f.Close()
	}
	t.Setenv("SHELL", "/bin/sh")
	srv, accepted, closed := holdingTerminalServer(t)
	a := testApp(t, "wss://"+srv.Listener.Addr().String()+"/ws", poolFor(srv), proto.CapTerminal)
	defer a.Close(5 * time.Second)

	a.OnConnected(proto.HelloAck{}, make(fakeSender, 4))
	send := make(fakeSender, 4)
	a.OnMessage(terminalOpen(t), send)
	if res, _ := openResult(t, send, 10*time.Second); res.Result != "ok" {
		t.Fatalf("result %q, want ok", res.Result)
	}
	<-accepted

	// Network blip: the control connection drops and comes back.
	a.OnDisconnected()
	a.OnConnected(proto.HelloAck{}, make(fakeSender, 4))
	a.OnDisconnected()
	select {
	case <-closed:
		t.Fatal("terminal socket closed by a control reconnect")
	case <-time.After(500 * time.Millisecond):
	}
	if n := a.liveTerminalCount(); n != 1 {
		t.Fatalf("live %d, want 1", n)
	}

	// The next reconnect is rejected: the token was revoked.
	a.OnRejected(&transport.HandshakeError{Code: proto.ErrAuthFailed, Message: "handshake rejected"})
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("terminal socket not closed after auth rejection")
	}
	deadline := time.Now().Add(10 * time.Second)
	for a.liveTerminalCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("session not released after auth rejection")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
