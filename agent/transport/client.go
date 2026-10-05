// Package transport dials the control plane and keeps the connection alive.
package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/jhyoong/KumaBoard/proto"
)

// Sender writes envelopes to the current connection.
type Sender interface {
	Send(env *proto.Envelope) error
}

// Handler receives connection events and non-liveness messages.
type Handler interface {
	OnConnected(ack proto.HelloAck, send Sender)
	OnDisconnected()
	OnMessage(env *proto.Envelope, send Sender)
}

// RejectionHandler is optionally implemented by a Handler that wants to know
// why a handshake was refused. OnRejected is called on the Run goroutine
// with a *HandshakeError before the client backs off and redials.
type RejectionHandler interface {
	OnRejected(err error)
}

// ErrAuthRejected matches (via errors.Is) a *HandshakeError caused by the
// server no longer accepting this agent's credentials: an in-band auth_failed
// or unknown_device reply to hello, or HTTP 401/403 on the upgrade request.
// Network failures and other rejections (e.g. protocol version) never match.
var ErrAuthRejected = errors.New("transport: agent credentials rejected")

// HandshakeError describes a handshake the server refused. Either Code (an
// in-band proto.Error reply to hello) or HTTPStatus (upgrade refused) is set.
type HandshakeError struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *HandshakeError) Error() string {
	if e.HTTPStatus != 0 {
		return fmt.Sprintf("handshake rejected: HTTP %d", e.HTTPStatus)
	}
	return fmt.Sprintf("handshake rejected: %s: %s", e.Code, e.Message)
}

// Is reports whether e is an authentication rejection (target ErrAuthRejected).
func (e *HandshakeError) Is(target error) bool {
	if target != ErrAuthRejected {
		return false
	}
	switch {
	case e.HTTPStatus == http.StatusUnauthorized, e.HTTPStatus == http.StatusForbidden:
		return true
	case e.Code == proto.ErrAuthFailed, e.Code == proto.ErrUnknownDevice:
		return true
	}
	return false
}

// Client is the agent side of the control socket. Run never returns until
// ctx is cancelled; every failure leads to a reconnect.
type Client struct {
	URL     string
	TLS     *tls.Config
	Hello   func() proto.Hello
	Handler Handler
	Log     *slog.Logger

	// Tunables with defaults set in Run.
	DialTimeout    time.Duration // 15s
	SilenceTimeout time.Duration // 45s
	SleepSlack     time.Duration // 30s
	Now            func() time.Time
	Backoff        *Backoff
}

type reason int

const (
	reasonDialFailed reason = iota
	reasonError
	reasonSilence
	reasonSleep
	reasonRejected
)

func (c *Client) defaults() {
	if c.DialTimeout == 0 {
		c.DialTimeout = 15 * time.Second
	}
	if c.SilenceTimeout == 0 {
		c.SilenceTimeout = 45 * time.Second
	}
	if c.SleepSlack == 0 {
		c.SleepSlack = 30 * time.Second
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Backoff == nil {
		c.Backoff = NewBackoff(time.Second, 60*time.Second, 0.2)
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
}

// Run is the agent's main loop.
func (c *Client) Run(ctx context.Context) {
	c.defaults()
	for {
		r := c.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		var wait time.Duration
		switch r {
		case reasonSleep:
			c.Backoff.Reset()
			c.Log.Info("resume from sleep detected; redialing now")
		case reasonRejected:
			c.Backoff.Saturate()
			wait = c.Backoff.Next()
		default:
			wait = c.Backoff.Next()
		}
		if wait > 0 {
			c.Log.Info("reconnecting", "in", wait.Round(time.Millisecond))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (c *Client) runOnce(ctx context.Context) reason {
	dctx, cancel := context.WithTimeout(ctx, c.DialTimeout)
	conn, httpResp, err := websocket.Dial(dctx, c.URL, &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: c.TLS}},
	})
	cancel()
	if err != nil {
		if st := statusOf(httpResp); st == http.StatusUnauthorized || st == http.StatusForbidden {
			c.Log.Error("handshake rejected by server", "http_status", st)
			c.rejected(&HandshakeError{HTTPStatus: st})
			return reasonRejected
		}
		c.Log.Warn("dial failed", "url", c.URL, "err", err)
		return reasonDialFailed
	}
	conn.SetReadLimit(proto.MaxMessageSize)
	defer conn.CloseNow()
	s := &session{conn: conn}

	hello, _ := proto.New(proto.TypeHello, c.Hello())
	if err := s.Send(hello); err != nil {
		return reasonError
	}
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	resp, err := s.read(hctx)
	cancel()
	if err != nil {
		c.Log.Warn("no handshake response", "err", err)
		return reasonError
	}
	var ack proto.HelloAck
	switch resp.Type {
	case proto.TypeHelloAck:
		if err := resp.Unmarshal(&ack); err != nil {
			return reasonError
		}
	case proto.TypeError:
		var e proto.Error
		resp.Unmarshal(&e)
		c.Log.Error("handshake rejected by server", "code", e.Code, "message", e.Message)
		c.rejected(&HandshakeError{Code: e.Code, Message: e.Message})
		return reasonRejected
	default:
		return reasonError
	}
	c.Backoff.Reset()
	hb := time.Duration(ack.HeartbeatIntervalS) * time.Second
	if hb <= 0 {
		hb = 15 * time.Second
	}
	c.Log.Info("connected", "session", ack.SessionID, "metrics_interval_s", ack.MetricsIntervalS)
	c.Handler.OnConnected(ack, s)
	defer c.Handler.OnDisconnected()

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan reason, 2)

	go func() { // sleep detector
		last := c.Now()
		t := time.NewTicker(hb)
		defer t.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-t.C:
				now := c.Now()
				if sleepDetected(last, now, hb, c.SleepSlack) {
					result <- reasonSleep
					conn.Close(websocket.StatusGoingAway, "resume from sleep")
					return
				}
				last = now
			}
		}
	}()

	go func() { // reader
		for {
			rctx, cancel := context.WithTimeout(sctx, c.SilenceTimeout)
			env, err := s.read(rctx)
			cancel()
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) && sctx.Err() == nil {
					c.Log.Warn("no traffic from server; reconnecting", "silence", c.SilenceTimeout)
					result <- reasonSilence
				} else {
					c.Log.Warn("connection closed", "err", err)
					result <- reasonError
				}
				return
			}
			if env.Type == proto.TypePing {
				pong, _ := proto.Reply(env, proto.TypePong, nil)
				s.Send(pong)
				continue
			}
			c.Handler.OnMessage(env, s)
		}
	}()

	select {
	case r := <-result:
		return r
	case <-ctx.Done():
		return reasonError
	}
}

func statusOf(r *http.Response) int {
	if r == nil {
		return 0
	}
	return r.StatusCode
}

func (c *Client) rejected(err *HandshakeError) {
	if rh, ok := c.Handler.(RejectionHandler); ok {
		rh.OnRejected(err)
	}
}

type session struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

// Send writes one envelope. When it returns without error the frame has been
// handed to the kernel, which is what expect_disconnect commands rely on.
func (s *session) Send(env *proto.Envelope) error {
	b, err := proto.Encode(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.conn.Write(ctx, websocket.MessageText, b)
}

func (s *session) read(ctx context.Context) (*proto.Envelope, error) {
	_, data, err := s.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	return proto.Decode(data)
}
