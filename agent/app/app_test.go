package app

import (
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jhyoong/KumaBoard/agent/config"
	agentterm "github.com/jhyoong/KumaBoard/agent/terminal"
	"github.com/jhyoong/KumaBoard/agent/upgrade"
	"github.com/jhyoong/KumaBoard/proto"
)

type fakeSender chan *proto.Envelope

func (f fakeSender) Send(env *proto.Envelope) error {
	f <- env
	return nil
}

func testApp(t *testing.T, serverURL string, pool *x509.CertPool, caps ...string) *App {
	t.Helper()
	cfg := &config.Config{Capabilities: caps, Token: "device-token", CAPool: pool}
	cfg.Device.Name = "dev"
	cfg.Server.URL = serverURL
	return New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), upgrade.StartupResult{})
}

func terminalOpen(t *testing.T) *proto.Envelope {
	t.Helper()
	env, err := proto.New(proto.TypeTerminalOpen, proto.TerminalOpen{
		SessionID: "sess-1", AgentTicket: "agent-ticket", Cols: 80, Rows: 24,
	})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func openResult(t *testing.T, send fakeSender, within time.Duration) (proto.TerminalOpenResult, time.Time) {
	t.Helper()
	select {
	case env := <-send:
		at := time.Now()
		if env.Type != proto.TypeTerminalOpenResult {
			t.Fatalf("reply type %q", env.Type)
		}
		var res proto.TerminalOpenResult
		if err := env.Unmarshal(&res); err != nil {
			t.Fatal(err)
		}
		if res.SessionID != "sess-1" {
			t.Fatalf("session_id %q", res.SessionID)
		}
		return res, at
	case <-time.After(within):
		t.Fatal("no terminal_open_result")
	}
	return proto.TerminalOpenResult{}, time.Time{}
}

// terminalServer is a TLS stand-in for the control plane that counts every
// request and accepts /ws/terminal after acceptDelay.
func terminalServer(t *testing.T, acceptDelay time.Duration) (srv *httptest.Server, hits *atomic.Int32, accepted chan time.Time, hellos chan proto.TerminalHello) {
	t.Helper()
	hits = &atomic.Int32{}
	accepted = make(chan time.Time, 1)
	hellos = make(chan proto.TerminalHello, 1)
	stop := make(chan struct{})
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("terminal socket carried Authorization")
		}
		time.Sleep(acceptDelay)
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		accepted <- time.Now()
		_, data, err := c.Read(r.Context())
		if err != nil {
			return
		}
		var h proto.TerminalHello
		json.Unmarshal(data, &h)
		hellos <- h
		<-stop
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stop) })
	return srv, hits, accepted, hellos
}

func poolFor(srv *httptest.Server) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return pool
}

// TestTerminalOpenRefusedWithoutLocalCapability is the daily-driver guard: a
// terminal_open from the server is refused, and nothing is dialed, when the
// local config lacks the terminal capability.
func TestTerminalOpenRefusedWithoutLocalCapability(t *testing.T) {
	srv, hits, _, _ := terminalServer(t, 0)
	a := testApp(t, "wss://"+srv.Listener.Addr().String()+"/ws", poolFor(srv), proto.CapMetrics)
	send := make(fakeSender, 4)
	a.OnMessage(terminalOpen(t), send)
	res, _ := openResult(t, send, time.Second)
	if res.Result != "refused" {
		t.Fatalf("result %q, want refused", res.Result)
	}
	time.Sleep(200 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Fatalf("agent dialed the server %d times after refusing", n)
	}
}

// TestTerminalOpenRefusedWhenDialFails checks that a failed terminal socket
// dial is reported as refused rather than ok.
func TestTerminalOpenRefusedWhenDialFails(t *testing.T) {
	srv, _, _, _ := terminalServer(t, 0)
	a := testApp(t, "wss://127.0.0.1:1/ws", poolFor(srv), proto.CapTerminal)
	send := make(fakeSender, 4)
	a.OnMessage(terminalOpen(t), send)
	if res, _ := openResult(t, send, 20*time.Second); res.Result != "refused" {
		t.Fatalf("result %q, want refused", res.Result)
	}
}

// TestTerminalOpenRefusedWithUntrustedCA checks the terminal socket is only
// dialed with the pinned CA: a server with an unknown certificate is refused.
func TestTerminalOpenRefusedWithUntrustedCA(t *testing.T) {
	srv, _, _, _ := terminalServer(t, 0)
	a := testApp(t, "wss://"+srv.Listener.Addr().String()+"/ws", x509.NewCertPool(), proto.CapTerminal)
	send := make(fakeSender, 4)
	a.OnMessage(terminalOpen(t), send)
	if res, _ := openResult(t, send, 20*time.Second); res.Result != "refused" {
		t.Fatalf("result %q, want refused", res.Result)
	}
}

// TestTerminalOpenOKOnlyAfterHello checks ok is sent only once the terminal
// socket is up and the hello frame written.
func TestTerminalOpenOKOnlyAfterHello(t *testing.T) {
	if !agentterm.Supported {
		t.Skip("terminal unsupported on this platform")
	}
	if f, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0); err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	} else {
		f.Close()
	}
	t.Setenv("SHELL", "/bin/sh")
	srv, _, accepted, hellos := terminalServer(t, 300*time.Millisecond)
	a := testApp(t, "wss://"+srv.Listener.Addr().String()+"/ws", poolFor(srv), proto.CapTerminal)
	defer a.Close(5 * time.Second)
	send := make(fakeSender, 4)
	a.OnMessage(terminalOpen(t), send)

	res, okAt := openResult(t, send, 10*time.Second)
	if res.Result != "ok" {
		t.Fatalf("result %q, want ok", res.Result)
	}
	acceptedAt := <-accepted
	if okAt.Before(acceptedAt) {
		t.Fatalf("ok sent %s before the terminal socket was accepted", acceptedAt.Sub(okAt))
	}
	select {
	case h := <-hellos:
		if h.SessionID != "sess-1" || h.AgentTicket != "agent-ticket" {
			t.Fatalf("hello %+v", h)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no hello frame")
	}
}
