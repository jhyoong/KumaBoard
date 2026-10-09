package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jhyoong/KumaBoard/proto"
)

type connectFailure struct {
	stage string
	err   error
}

type failureRecorder struct{ failures chan connectFailure }

func (r *failureRecorder) OnConnected(proto.HelloAck, Sender) {}
func (r *failureRecorder) OnDisconnected()                    {}
func (r *failureRecorder) OnMessage(*proto.Envelope, Sender)  {}
func (r *failureRecorder) OnConnectFailed(stage string, err error) {
	select {
	case r.failures <- connectFailure{stage, err}:
	default:
	}
}

func failingClient(url string, tlsCfg *tls.Config, h Handler) *Client {
	return &Client{
		URL:     url,
		TLS:     tlsCfg,
		Hello:   func() proto.Hello { return proto.Hello{DeviceName: "dev", Token: "t"} },
		Handler: h,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Backoff: NewBackoff(10*time.Millisecond, 20*time.Millisecond, 0),
	}
}

// firstConnectFailure runs a client against h and returns the first failure
// it reports.
func firstConnectFailure(t *testing.T, h http.Handler, trustServer bool) connectFailure {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	defer srv.Close()
	pool := x509.NewCertPool()
	if trustServer {
		pool.AddCert(srv.Certificate())
	}
	rec := &failureRecorder{failures: make(chan connectFailure, 8)}
	c := failingClient("wss://"+srv.Listener.Addr().String()+"/ws", &tls.Config{RootCAs: pool}, rec)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	select {
	case f := <-rec.failures:
		return f
	case <-time.After(15 * time.Second):
		t.Fatal("OnConnectFailed not called")
	}
	return connectFailure{}
}

func TestConnectFailedDial(t *testing.T) {
	// The server's certificate is not trusted, so the TLS handshake fails.
	f := firstConnectFailure(t, http.NotFoundHandler(), false)
	if f.stage != ConnectStageDial || f.err == nil {
		t.Fatalf("failure = %q %v, want stage dial", f.stage, f.err)
	}
}

func TestConnectFailedHello(t *testing.T) {
	// The server accepts the socket, reads hello and hangs up without a reply.
	f := firstConnectFailure(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.Read(r.Context())
	}), true)
	if f.stage != ConnectStageHello || f.err == nil {
		t.Fatalf("failure = %q %v, want stage hello", f.stage, f.err)
	}
}

func TestConnectFailedRejected(t *testing.T) {
	f := firstConnectFailure(t, inBandRejector(proto.ErrProtocolVersionUnsupported), true)
	if f.stage != ConnectStageRejected {
		t.Fatalf("stage = %q, want rejected", f.stage)
	}
	var he *HandshakeError
	if !errors.As(f.err, &he) || he.Code != proto.ErrProtocolVersionUnsupported {
		t.Fatalf("err = %#v, want the HandshakeError", f.err)
	}

	f = firstConnectFailure(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}), true)
	if f.stage != ConnectStageRejected || !errors.Is(f.err, ErrAuthRejected) {
		t.Fatalf("failure = %q %v, want stage rejected with ErrAuthRejected", f.stage, f.err)
	}
}

// plainHandler implements Handler and nothing else.
type plainHandler struct{}

func (plainHandler) OnConnected(proto.HelloAck, Sender) {}
func (plainHandler) OnDisconnected()                    {}
func (plainHandler) OnMessage(*proto.Envelope, Sender)  {}

// A Handler without OnConnectFailed keeps working: the client fails every
// stage and keeps redialing without panicking.
func TestConnectFailureHandlerOptional(t *testing.T) {
	for name, tc := range map[string]struct {
		h     http.Handler
		trust bool
	}{
		"dial":     {http.NotFoundHandler(), false},
		"rejected": {inBandRejector(proto.ErrAuthFailed), true},
	} {
		srv := httptest.NewTLSServer(tc.h)
		pool := x509.NewCertPool()
		if tc.trust {
			pool.AddCert(srv.Certificate())
		}
		c := failingClient("wss://"+srv.Listener.Addr().String()+"/ws", &tls.Config{RootCAs: pool}, plainHandler{})
		c.defaults()
		want := reasonDialFailed
		if name == "rejected" {
			want = reasonRejected
		}
		if got := c.runOnce(context.Background()); got != want {
			t.Errorf("%s: runOnce = %d, want %d", name, got, want)
		}
		srv.Close()
	}
}
