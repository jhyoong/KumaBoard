package hub

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

type recorder struct {
	mu     sync.Mutex
	events []string
	output []recordedOutput
}

type recordedOutput struct {
	runID string
	seq   int
	o     proto.CommandOutput
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, s)
}
func (r *recorder) DeviceConnected(name string)                  { r.add("connect:" + name) }
func (r *recorder) DeviceDisconnected(name string)               { r.add("disconnect:" + name) }
func (r *recorder) MetricsReceived(name string, m proto.Metrics) { r.add("metrics:" + name) }
func (r *recorder) RunChanged(runID string)                      { r.add("run:" + runID) }
func (r *recorder) UpgradeChanged(name string)                   { r.add("upgrade:" + name) }
func (r *recorder) CommandsChanged(name string)                  { r.add("commands:" + name) }
func (r *recorder) RunOutput(runID, device string, seq int, o proto.CommandOutput) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.output = append(r.output, recordedOutput{runID, seq, o})
}
func (r *recorder) has(s string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e == s {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func newTestHub(t *testing.T, opts Options) (*Hub, *store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	opts.Store = st
	if opts.Events == nil {
		opts.Events = &recorder{}
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if opts.MetricsInterval == 0 {
		opts.MetricsInterval = 30 * time.Second
	}
	if opts.HeartbeatInterval == 0 {
		opts.HeartbeatInterval = 15 * time.Second
	}
	h := New(opts)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return h, st, "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dialHello(t *testing.T, url string, hello proto.Hello) (*websocket.Conn, *proto.Envelope) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := proto.New(proto.TypeHello, hello)
	b, _ := proto.Encode(env)
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := proto.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	return conn, resp
}

func goodHello(name, token string) proto.Hello {
	return proto.Hello{ProtocolVersion: proto.Version, AgentVersion: "0.1.0", DeviceName: name,
		Token: token, OS: "linux", Arch: "amd64", Capabilities: []string{"metrics"}}
}

func errCode(t *testing.T, env *proto.Envelope) string {
	t.Helper()
	if env.Type != proto.TypeError {
		t.Fatalf("want error, got %s", env.Type)
	}
	var e proto.Error
	env.Unmarshal(&e)
	return e.Code
}

func TestHandshakeOK(t *testing.T) {
	rec := &recorder{}
	h, st, url := newTestHub(t, Options{Events: rec})
	_, token, _ := st.CreateDevice(context.Background(), "dev", "", false, store.Schedule{})
	conn, resp := dialHello(t, url, goodHello("dev", token))
	defer conn.CloseNow()
	if resp.Type != proto.TypeHelloAck {
		t.Fatalf("want hello_ack, got %s", resp.Type)
	}
	var ack proto.HelloAck
	resp.Unmarshal(&ack)
	if ack.SessionID == "" || ack.MetricsIntervalS != 30 || ack.HeartbeatIntervalS != 15 {
		t.Fatalf("bad ack: %+v", ack)
	}
	waitFor(t, func() bool { return rec.has("connect:dev") && h.Connected("dev") })
	d, _ := st.GetDevice(context.Background(), "dev")
	if d.AgentVersion != "0.1.0" || d.OS != "linux" {
		t.Fatalf("handshake not recorded: %+v", d)
	}
}

func TestHandshakeErrors(t *testing.T) {
	_, st, url := newTestHub(t, Options{})
	_, token, _ := st.CreateDevice(context.Background(), "dev", "", false, store.Schedule{})

	_, resp := dialHello(t, url, goodHello("nope", token))
	if c := errCode(t, resp); c != proto.ErrUnknownDevice {
		t.Fatalf("got %s", c)
	}
	_, resp = dialHello(t, url, goodHello("dev", "wrong"))
	if c := errCode(t, resp); c != proto.ErrAuthFailed {
		t.Fatalf("got %s", c)
	}
	bad := goodHello("dev", token)
	bad.ProtocolVersion = 99
	_, resp = dialHello(t, url, bad)
	if c := errCode(t, resp); c != proto.ErrProtocolVersionUnsupported {
		t.Fatalf("got %s", c)
	}
	d, _ := st.GetDevice(context.Background(), "dev")
	if d.LastRejectReason != proto.ErrProtocolVersionUnsupported {
		t.Fatalf("reject reason not recorded: %q", d.LastRejectReason)
	}
	st.RevokeToken(context.Background(), "dev")
	_, resp = dialHello(t, url, goodHello("dev", token))
	if c := errCode(t, resp); c != proto.ErrAuthFailed {
		t.Fatalf("revoked token accepted: %s", c)
	}
}

func TestDuplicateSessionReplacesOld(t *testing.T) {
	rec := &recorder{}
	h, st, url := newTestHub(t, Options{Events: rec})
	_, token, _ := st.CreateDevice(context.Background(), "dev", "", false, store.Schedule{})
	c1, _ := dialHello(t, url, goodHello("dev", token))
	c2, resp := dialHello(t, url, goodHello("dev", token))
	defer c2.CloseNow()
	if resp.Type != proto.TypeHelloAck {
		t.Fatalf("second session refused: %s", resp.Type)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := c1.Read(ctx); err == nil {
		t.Fatal("old connection still open")
	}
	waitFor(t, func() bool { return h.SessionCount() == 1 && h.Connected("dev") })
	if rec.has("disconnect:dev") {
		t.Fatal("replacement must not emit a disconnect for the device")
	}
}

func TestPongTimeoutCloses(t *testing.T) {
	_, st, url := newTestHub(t, Options{HeartbeatInterval: 50 * time.Millisecond, PongTimeout: 50 * time.Millisecond})
	_, token, _ := st.CreateDevice(context.Background(), "dev", "", false, store.Schedule{})
	conn, _ := dialHello(t, url, goodHello("dev", token))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Read pings but never answer. The server must close us.
	for {
		_, _, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				t.Fatal("server did not close a client that never pongs")
			}
			return
		}
	}
}

// manualDeadlines replaces pong-deadline timers with callbacks the test fires
// itself, one per ping in send order.
func manualDeadlines() (func(time.Duration, func()), <-chan func()) {
	ch := make(chan func(), 1024)
	return func(_ time.Duration, f func()) {
		select {
		case ch <- f:
		default: // never fired, so never closes
		}
	}, ch
}

func nextDeadline(t *testing.T, deadlines <-chan func()) func() {
	t.Helper()
	select {
	case f := <-deadlines:
		return f
	case <-time.After(3 * time.Second):
		t.Fatal("no ping deadline scheduled")
		return nil
	}
}

func TestPongKeepsAlive(t *testing.T) {
	after, deadlines := manualDeadlines()
	h, st, url := newTestHub(t, Options{HeartbeatInterval: 10 * time.Millisecond, PongTimeout: time.Nanosecond, afterFunc: after})
	_, token, _ := st.CreateDevice(context.Background(), "dev", "", false, store.Schedule{})
	conn, _ := dialHello(t, url, goodHello("dev", token))
	defer conn.CloseNow()
	// Answer every ping until the test ends. The read context must outlive
	// the assertions: cancelling a pending Read closes the websocket.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			env, _ := proto.Decode(data)
			if env.Type == proto.TypePing {
				pong, _ := proto.Reply(env, proto.TypePong, nil)
				b, _ := proto.Encode(pong)
				conn.Write(ctx, websocket.MessageText, b)
			}
		}
	}()
	var sess *Session
	waitFor(t, func() bool { sess = h.Session("dev"); return sess != nil })
	for i := uint64(1); i <= 5; i++ {
		fire := nextDeadline(t, deadlines)
		// Deadline i belongs to ping i, and pong i can only be counted
		// after ping i was sent.
		waitFor(t, func() bool { return sess.pongs.Load() >= i })
		fire()
		select {
		case <-sess.Closed():
			t.Fatalf("session closed at ping %d deadline despite pong", i)
		default:
		}
	}
	if !h.Connected("dev") {
		t.Fatal("session dropped despite pongs")
	}
}

func TestPongDeadlineClosesUnansweredPing(t *testing.T) {
	after, deadlines := manualDeadlines()
	_, st, url := newTestHub(t, Options{HeartbeatInterval: 10 * time.Millisecond, PongTimeout: time.Hour, afterFunc: after})
	_, token, _ := st.CreateDevice(context.Background(), "dev", "", false, store.Schedule{})
	conn, _ := dialHello(t, url, goodHello("dev", token))
	defer conn.CloseNow()
	nextDeadline(t, deadlines)()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("server did not close after an unanswered ping deadline")
			}
			return
		}
	}
}

func TestOversizeMessageCloses(t *testing.T) {
	_, st, url := newTestHub(t, Options{})
	_, token, _ := st.CreateDevice(context.Background(), "dev", "", false, store.Schedule{})
	conn, _ := dialHello(t, url, goodHello("dev", token))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	big := []byte(`{"v":1,"id":"x","type":"metrics","payload":"` + strings.Repeat("a", proto.MaxMessageSize) + `"}`)
	conn.Write(ctx, websocket.MessageText, big)
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("server accepted an oversized frame")
	}
}

func TestTerminalRefusalNotifiesBroker(t *testing.T) {
	type refusal struct{ dev, sid string }
	got := make(chan refusal, 2)
	_, st, url := newTestHub(t, Options{TerminalRefused: func(dev, sid string) { got <- refusal{dev, sid} }})
	_, token, _ := st.CreateDevice(context.Background(), "dev", "", false, store.Schedule{})
	conn, _ := dialHello(t, url, goodHello("dev", token))
	defer conn.CloseNow()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, res := range []string{"ok", "refused"} {
		env, _ := proto.New(proto.TypeTerminalOpenResult, proto.TerminalOpenResult{SessionID: "sess-" + res, Result: res})
		b, _ := proto.Encode(env)
		if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case r := <-got:
		if r.dev != "dev" || r.sid != "sess-refused" {
			t.Fatalf("got %+v, want dev/sess-refused", r)
		}
	case <-ctx.Done():
		t.Fatal("refusal not forwarded")
	}
	select {
	case r := <-got:
		t.Fatalf("unexpected extra callback %+v", r)
	default:
	}
}
