//go:build !windows

package terminal

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jhyoong/KumaBoard/proto"
)

func requirePTY(t *testing.T) {
	t.Helper()
	f, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	}
	f.Close()
	// Portable syntax in every test script; the login-shell path is the same.
	t.Setenv("SHELL", "/bin/sh")
}

// output accumulates everything read from r.
type output struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

// waitMatch polls o until re matches, returning the submatches.
func (o *output) waitMatch(t *testing.T, re *regexp.Regexp, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m := re.FindStringSubmatch(o.String()); m != nil {
			return m
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no match for %s in output:\n%s", re, o.String())
	return nil
}

func startTestShell(t *testing.T) (*Shell, *output) {
	t.Helper()
	return startShellAs(t, "/bin/sh")
}

// startShellAs spawns the production shell with $SHELL set to shell.
func startShellAs(t *testing.T, shell string) (*Shell, *output) {
	t.Helper()
	requirePTY(t)
	t.Setenv("SHELL", shell)
	sh, err := StartShell(80, 24)
	if err != nil {
		t.Fatalf("StartShell: %v", err)
	}
	t.Cleanup(sh.Close)
	out := &output{}
	go io.Copy(out, sh.PTY)
	return sh, out
}

func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil && !isZombie(pid)
}

func waitDead(t *testing.T, pid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("pid %d still alive after %s", pid, within)
}

// Markers are written as KB_%s with printf so the echoed input never matches.
func TestStartShellEchoRoundTrip(t *testing.T) {
	sh, out := startTestShell(t)
	sh.PTY.Write([]byte("printf 'KB_%s\\n' ROUNDTRIP\n"))
	out.waitMatch(t, regexp.MustCompile(`KB_ROUNDTRIP`), 5*time.Second)
}

func TestShellResize(t *testing.T) {
	sh, out := startTestShell(t)
	if err := sh.Resize(123, 45); err != nil {
		t.Fatal(err)
	}
	sh.PTY.Write([]byte("echo SZ_$(stty size | tr ' ' x)\n"))
	out.waitMatch(t, regexp.MustCompile(`SZ_45x123`), 5*time.Second)
}

func TestShellExitCode(t *testing.T) {
	sh, _ := startTestShell(t)
	sh.PTY.Write([]byte("exit 7\n"))
	select {
	case <-sh.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("shell did not exit")
	}
	if code := sh.ExitCode(); code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}
}

// TestShellHygiene checks the login shell, the minimal environment, and the
// working directory.
func TestShellHygiene(t *testing.T) {
	t.Setenv("KUMA_AGENT_CONFIG", "/etc/kuma-agent/config.yaml")
	t.Setenv("SOME_API_TOKEN", "hunter2")
	t.Setenv("LANG", "C.UTF-8")
	sh, out := startTestShell(t)
	home, _ := account()
	sh.PTY.Write([]byte("echo ARGV0_$0; echo CWD_$(pwd); echo TERM_$TERM; echo LANG_$LANG; env | sed 's/^/ENV_/'; printf 'KB_%s\\n' DONE\n"))
	out.waitMatch(t, regexp.MustCompile(`KB_DONE`), 5*time.Second)
	got := out.String()
	for _, want := range []string{"ARGV0_-sh", "CWD_" + home, "TERM_xterm-256color", "LANG_C.UTF-8"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"ENV_KUMA_", "hunter2"} {
		if strings.Contains(got, bad) {
			t.Errorf("output leaks %q:\n%s", bad, got)
		}
	}
}

// TestShellCloseKillsSession spawns background jobs (their own process groups
// under job control, one ignoring SIGHUP/SIGTERM) and a foreground cat, then
// closes the shell: the shell and every descendant must die within 5s.
func TestShellCloseKillsSession(t *testing.T) {
	for _, shell := range []string{"/bin/sh", "/bin/bash"} {
		t.Run(shell, func(t *testing.T) {
			if _, err := os.Stat(shell); err != nil {
				t.Skipf("no %s", shell)
			}
			testShellCloseKillsSession(t, shell)
		})
	}
}

func testShellCloseKillsSession(t *testing.T, shell string) {
	sh, out := startShellAs(t, shell)
	// IG ignores SIGHUP and SIGTERM, so only the SIGKILL escalation stops it.
	sh.PTY.Write([]byte("set -m; sleep 300 & echo BG_$!; (trap '' HUP TERM; exec sleep 301) & echo IG_$!; echo SH_$$; cat\n"))
	bg, _ := strconv.Atoi(out.waitMatch(t, regexp.MustCompile(`BG_(\d+)`), 5*time.Second)[1])
	ig, _ := strconv.Atoi(out.waitMatch(t, regexp.MustCompile(`IG_(\d+)`), 5*time.Second)[1])
	shellPID, _ := strconv.Atoi(out.waitMatch(t, regexp.MustCompile(`SH_(\d+)`), 5*time.Second)[1])
	if shellPID != sh.Cmd.Process.Pid {
		t.Fatalf("shell pid %d, cmd pid %d", shellPID, sh.Cmd.Process.Pid)
	}
	if !alive(bg) {
		t.Fatal("background job not running")
	}
	// Under job control the job has its own group, so killing only the
	// shell's group would miss it.
	if pgid, _ := syscall.Getpgid(bg); pgid == shellPID {
		t.Fatalf("background job pgid %d is the shell's; test would not cover session kill", pgid)
	}
	start := time.Now()
	sh.Close()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Close took %s", d)
	}
	waitDead(t, shellPID, time.Second)
	waitDead(t, bg, time.Second)
	waitDead(t, ig, time.Second)
	if pids := sessionPIDs(shellPID); len(pids) != 0 {
		t.Fatalf("session still has processes: %v", pids)
	}
}

func TestSelfTest(t *testing.T) {
	requirePTY(t)
	if err := SelfTest(10 * time.Second); err != nil {
		t.Fatal(err)
	}
}

// fakeBroker is a TLS /ws/terminal endpoint that hands each accepted agent
// socket, plus its first frame and headers, to the test.
type fakeBroker struct {
	srv   *httptest.Server
	conns chan brokerConn
	stop  chan struct{}
}

type brokerConn struct {
	conn   *websocket.Conn
	hello  proto.TerminalHello
	header http.Header
	err    error
}

func newFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	fb := &fakeBroker{conns: make(chan brokerConn, 1), stop: make(chan struct{})}
	fb.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		bc := brokerConn{conn: c, header: r.Header.Clone()}
		typ, data, err := c.Read(r.Context())
		if err == nil && typ != websocket.MessageText {
			err = errors.New("first frame is not text")
		}
		if err == nil {
			err = json.Unmarshal(data, &bc.hello)
		}
		bc.err = err
		fb.conns <- bc
		// Keep the handler alive while the test drives the socket.
		<-fb.stop
	}))
	t.Cleanup(fb.srv.Close)
	t.Cleanup(func() { close(fb.stop) })
	return fb
}

func (fb *fakeBroker) session(url string) *Session {
	pool := x509.NewCertPool()
	pool.AddCert(fb.srv.Certificate())
	return &Session{
		URL:       url,
		TLS:       &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		SessionID: "sess-1",
		Ticket:    "agent-ticket-1",
		Cols:      80,
		Rows:      24,
	}
}

func (fb *fakeBroker) url() string {
	return "wss://" + fb.srv.Listener.Addr().String() + "/ws/terminal"
}

func (fb *fakeBroker) accept(t *testing.T) brokerConn {
	t.Helper()
	select {
	case bc := <-fb.conns:
		if bc.err != nil {
			t.Fatalf("hello: %v", bc.err)
		}
		return bc
	case <-time.After(5 * time.Second):
		t.Fatal("agent never dialed")
	}
	return brokerConn{}
}

// readBinaryUntil reads frames from c until the binary stream matches re.
// Text frames are returned through ctrl.
func readBinaryUntil(t *testing.T, c *websocket.Conn, re *regexp.Regexp) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var seen []byte
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v (seen %q)", err, seen)
		}
		if typ == websocket.MessageBinary {
			seen = append(seen, data...)
			if m := re.FindSubmatch(seen); m != nil {
				out := make([]string, len(m))
				for i := range m {
					out[i] = string(m[i])
				}
				return out
			}
		}
	}
}

// TestSessionRelay drives a real session over TLS: hello is the first frame
// and carries no device token, bytes relay both ways, resize applies, and the
// shell's exit code arrives as an exit control frame.
func TestSessionRelay(t *testing.T) {
	requirePTY(t)
	fb := newFakeBroker(t)
	sess := fb.session(fb.url())
	if err := sess.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	served := make(chan struct{})
	go func() { sess.Serve(context.Background()); close(served) }()

	bc := fb.accept(t)
	if bc.hello.SessionID != "sess-1" || bc.hello.AgentTicket != "agent-ticket-1" {
		t.Fatalf("hello = %+v", bc.hello)
	}
	if h := bc.header.Get("Authorization"); h != "" {
		t.Fatalf("terminal socket carried Authorization %q", h)
	}
	ctx := context.Background()
	resize, _ := json.Marshal(proto.TerminalControl{Type: "resize", Cols: 132, Rows: 40})
	bc.conn.Write(ctx, websocket.MessageText, resize)
	bc.conn.Write(ctx, websocket.MessageBinary, []byte("echo SZ_$(stty size | tr ' ' x)\n"))
	readBinaryUntil(t, bc.conn, regexp.MustCompile(`SZ_40x132`))

	bc.conn.Write(ctx, websocket.MessageBinary, []byte("exit 3\n"))
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		typ, data, err := bc.conn.Read(rctx)
		if err != nil {
			t.Fatalf("no exit frame: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var ctrl proto.TerminalControl
		if err := json.Unmarshal(data, &ctrl); err != nil || ctrl.Type != "exit" {
			t.Fatalf("control frame %s", data)
		}
		if ctrl.Code != 3 {
			t.Fatalf("exit code = %d, want 3", ctrl.Code)
		}
		break
	}
	// The agent then closes normally; reading completes the close handshake.
	if _, _, err := bc.conn.Read(rctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("after exit: %v, want normal closure", err)
	}
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after shell exit")
	}
}

// TestSessionSocketCloseKillsShell closes the terminal socket from the server
// side (browser tab closed) and checks the shell and a background descendant
// die within 5s.
func TestSessionSocketCloseKillsShell(t *testing.T) {
	requirePTY(t)
	fb := newFakeBroker(t)
	sess := fb.session(fb.url())
	if err := sess.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	served := make(chan struct{})
	go func() { sess.Serve(context.Background()); close(served) }()
	bc := fb.accept(t)

	bc.conn.Write(context.Background(), websocket.MessageBinary, []byte("set -m; sleep 300 & echo BG_$!; echo SH_$$; cat\n"))
	m := readBinaryUntil(t, bc.conn, regexp.MustCompile(`BG_(\d+)[\s\S]*SH_(\d+)`))
	bg, _ := strconv.Atoi(m[1])
	shellPID, _ := strconv.Atoi(m[2])

	start := time.Now()
	bc.conn.Close(websocket.StatusNormalClosure, "tab closed")
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after socket close")
	}
	waitDead(t, shellPID, 5*time.Second-time.Since(start))
	waitDead(t, bg, time.Second)
}

// TestSessionOpenDialFailure checks a failed dial reports an error and leaves
// no shell behind.
func TestSessionOpenDialFailure(t *testing.T) {
	requirePTY(t)
	fb := newFakeBroker(t)
	sess := fb.session("wss://127.0.0.1:1/ws/terminal")
	if err := sess.Open(context.Background()); err == nil {
		t.Fatal("Open succeeded against a closed port")
	}
	if sess.shell != nil {
		t.Fatal("shell retained after failed open")
	}
}
