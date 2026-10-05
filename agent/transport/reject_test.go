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

func TestHandshakeErrorIsAuthRejected(t *testing.T) {
	cases := []struct {
		err  *HandshakeError
		want bool
	}{
		{&HandshakeError{Code: proto.ErrAuthFailed}, true},
		{&HandshakeError{Code: proto.ErrUnknownDevice}, true},
		{&HandshakeError{HTTPStatus: http.StatusUnauthorized}, true},
		{&HandshakeError{HTTPStatus: http.StatusForbidden}, true},
		{&HandshakeError{Code: proto.ErrProtocolVersionUnsupported}, false},
		{&HandshakeError{Code: proto.ErrProtocol}, false},
		{&HandshakeError{HTTPStatus: http.StatusBadGateway}, false},
	}
	for _, c := range cases {
		wrapped := errors.Join(errors.New("ctx"), c.err)
		if got := errors.Is(wrapped, ErrAuthRejected); got != c.want {
			t.Errorf("%v: errors.Is = %v, want %v", c.err, got, c.want)
		}
		var he *HandshakeError
		if !errors.As(wrapped, &he) || he != c.err {
			t.Errorf("%v: errors.As failed", c.err)
		}
	}
	if errors.Is(context.DeadlineExceeded, ErrAuthRejected) {
		t.Error("network error matched ErrAuthRejected")
	}
}

type rejectRecorder struct{ errs chan error }

func (r *rejectRecorder) OnConnected(proto.HelloAck, Sender) {}
func (r *rejectRecorder) OnDisconnected()                    {}
func (r *rejectRecorder) OnMessage(*proto.Envelope, Sender)  {}
func (r *rejectRecorder) OnRejected(err error)               { r.errs <- err }

func runRejectingClient(t *testing.T, h http.Handler) error {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	rec := &rejectRecorder{errs: make(chan error, 8)}
	c := &Client{
		URL:     "wss://" + srv.Listener.Addr().String() + "/ws",
		TLS:     &tls.Config{RootCAs: pool},
		Hello:   func() proto.Hello { return proto.Hello{DeviceName: "dev", Token: "t"} },
		Handler: rec,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Backoff: NewBackoff(10*time.Millisecond, 20*time.Millisecond, 0),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	select {
	case err := <-rec.errs:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("OnRejected not called")
	}
	return nil
}

func inBandRejector(code string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		_, data, err := c.Read(r.Context())
		if err != nil {
			return
		}
		hello, err := proto.Decode(data)
		if err != nil {
			return
		}
		e, _ := proto.Reply(hello, proto.TypeError, proto.Error{Code: code, Message: "handshake rejected"})
		b, _ := proto.Encode(e)
		c.Write(r.Context(), websocket.MessageText, b)
		c.Close(websocket.StatusPolicyViolation, code)
	})
}

func TestClientReportsInBandAuthRejection(t *testing.T) {
	err := runRejectingClient(t, inBandRejector(proto.ErrAuthFailed))
	if !errors.Is(err, ErrAuthRejected) {
		t.Fatalf("err %v, want ErrAuthRejected", err)
	}
	var he *HandshakeError
	if !errors.As(err, &he) || he.Code != proto.ErrAuthFailed {
		t.Fatalf("err %#v", err)
	}
}

func TestClientReportsNonAuthRejection(t *testing.T) {
	err := runRejectingClient(t, inBandRejector(proto.ErrProtocolVersionUnsupported))
	if errors.Is(err, ErrAuthRejected) {
		t.Fatalf("protocol rejection matched ErrAuthRejected: %v", err)
	}
}

func TestClientReportsHTTPAuthRejection(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		err := runRejectingClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "unauthorized", status)
		}))
		var he *HandshakeError
		if !errors.Is(err, ErrAuthRejected) || !errors.As(err, &he) || he.HTTPStatus != status {
			t.Fatalf("status %d: err %v", status, err)
		}
	}
}
