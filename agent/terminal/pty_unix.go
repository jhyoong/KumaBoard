//go:build !windows

package terminal

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/jhyoong/KumaBoard/proto"
)

// Supported reports whether this platform can run terminal sessions. When it
// is false, UnsupportedReason says why.
const Supported = true

// UnsupportedReason is empty on platforms where Supported is true.
const UnsupportedReason = ""

// defaultPath is used when the agent itself has no PATH (service managers).
const defaultPath = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

// Grace periods for tearing a shell down: SIGHUP/SIGTERM first, SIGKILL after
// termGrace, and give up waiting after killDeadline. killDeadline stays under
// the 5s the phase 3 acceptance criteria allow.
const (
	termGrace    = 1500 * time.Millisecond
	killDeadline = 4 * time.Second
)

// Shell is a login shell running on a PTY. It is the only spawn path: live
// sessions and the --selftest both go through StartShell.
type Shell struct {
	Cmd  *exec.Cmd
	PTY  *os.File
	done chan struct{}
	code int

	closeOnce sync.Once
}

// StartShell spawns the agent account's login shell on a new PTY of the given
// size. The shell gets a minimal environment, starts in the user's home
// directory, and leads its own session (creack/pty calls setsid), so every
// process it starts can be found and killed by session id.
func StartShell(cols, rows int) (*Shell, error) {
	shell := shellPath()
	home, username := account()
	cmd := exec.Command(shell)
	// A leading '-' in argv[0] makes it a login shell.
	cmd.Args = []string{"-" + filepath.Base(shell)}
	cmd.Env = shellEnv(shell, home, username)
	cmd.Dir = home
	// No Setpgid: StartWithSize sets Setsid+Setctty, and setpgid after
	// setsid fails with EPERM. The session leader is already its own group.
	cmd.SysProcAttr = &syscall.SysProcAttr{}

	ptmx, err := pty.StartWithSize(cmd, winsize(cols, rows))
	if err != nil {
		return nil, fmt.Errorf("terminal: start pty: %w", err)
	}
	s := &Shell{Cmd: cmd, PTY: ptmx, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			s.code = exit.ExitCode()
			if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				s.code = 128 + int(ws.Signal())
			}
		} else if err != nil {
			s.code = -1
		}
		close(s.done)
	}()
	return s, nil
}

// Resize sets the PTY window size.
func (s *Shell) Resize(cols, rows int) error {
	return pty.Setsize(s.PTY, winsize(cols, rows))
}

// Done is closed when the shell process has exited.
func (s *Shell) Done() <-chan struct{} { return s.done }

// ExitCode is valid after Done is closed. A shell killed by a signal reports
// 128+signal, as a shell would.
func (s *Shell) ExitCode() int {
	<-s.done
	return s.code
}

// Close kills every process in the shell's session (the shell, its process
// group, and background jobs in their own groups), closes the PTY master, and
// waits for the shell to be reaped. It returns within killDeadline plus a
// little slack. Safe to call more than once.
func (s *Shell) Close() {
	s.closeOnce.Do(func() {
		sid := s.Cmd.Process.Pid
		// SIGHUP is what a closing terminal sends; interactive shells ignore
		// SIGTERM but exit on SIGHUP. SIGCONT wakes stopped jobs so they see it.
		signalSession(sid, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGCONT)
		s.PTY.Close()

		deadline := time.Now().Add(killDeadline)
		graceEnd := time.Now().Add(termGrace)
		for {
			shellGone := false
			select {
			case <-s.done:
				shellGone = true
			default:
			}
			if shellGone && len(sessionPIDs(sid)) == 0 {
				return
			}
			if time.Now().After(graceEnd) {
				signalSession(sid, syscall.SIGKILL)
			}
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}

// signalSession sends each signal to the session leader's process group and
// to every process whose session id is sid.
func signalSession(sid int, sigs ...syscall.Signal) {
	pids := sessionPIDs(sid)
	for _, sig := range sigs {
		_ = syscall.Kill(-sid, sig)
		for _, pid := range pids {
			_ = syscall.Kill(pid, sig)
		}
	}
}

// sessionPIDs lists live processes in session sid. It reads /proc where it
// exists (Linux) and falls back to ps (macOS).
func sessionPIDs(sid int) []int {
	var out []int
	for _, pid := range allPIDs() {
		if pid <= 1 {
			continue
		}
		if got, err := unix.Getsid(pid); err == nil && got == sid && !isZombie(pid) {
			out = append(out, pid)
		}
	}
	return out
}

func allPIDs() []int {
	var pids []int
	if ents, err := os.ReadDir("/proc"); err == nil && len(ents) > 0 {
		for _, e := range ents {
			if pid, err := strconv.Atoi(e.Name()); err == nil {
				pids = append(pids, pid)
			}
		}
		return pids
	}
	b, err := exec.Command("ps", "-A", "-o", "pid=").Output()
	if err != nil {
		return nil
	}
	for _, f := range strings.Fields(string(b)) {
		if pid, err := strconv.Atoi(f); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// isZombie reports whether pid has exited but not been reaped. Orphans are
// reaped by init, which may lag; a zombie runs nothing and needs no kill.
func isZombie(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// Format: pid (comm) state ...; comm may contain spaces or ')'.
	i := strings.LastIndexByte(string(b), ')')
	return i >= 0 && i+2 < len(b) && b[i+2] == 'Z'
}

func winsize(cols, rows int) *pty.Winsize {
	if cols <= 0 || cols > 1000 {
		cols = 80
	}
	if rows <= 0 || rows > 1000 {
		rows = 24
	}
	return &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}
}

// shellPath picks $SHELL when it names an executable, else /bin/bash, else
// /bin/sh. Service managers usually leave $SHELL unset.
func shellPath() string {
	for _, p := range []string{os.Getenv("SHELL"), "/bin/bash", "/bin/sh"} {
		if p == "" || !filepath.IsAbs(p) {
			continue
		}
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return "/bin/sh"
}

// account returns the agent account's home directory and user name.
func account() (home, username string) {
	if u, err := user.Current(); err == nil {
		home, username = u.HomeDir, u.Username
	}
	if home == "" {
		home = os.Getenv("HOME")
	}
	if fi, err := os.Stat(home); home == "" || err != nil || !fi.IsDir() {
		home = "/"
	}
	if username == "" {
		username = strconv.Itoa(os.Getuid())
	}
	return home, username
}

// shellEnv is an allowlist: nothing from the agent's environment reaches the
// shell except PATH and LANG, so KUMA_* and any secrets are never inherited.
func shellEnv(shell, home, username string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = defaultPath
	}
	env := []string{
		"PATH=" + path,
		"HOME=" + home,
		"USER=" + username,
		"LOGNAME=" + username,
		"SHELL=" + shell,
		"TERM=xterm-256color",
	}
	if lang := os.Getenv("LANG"); lang != "" {
		env = append(env, "LANG="+lang)
	}
	return env
}

// Session holds everything needed to run a single terminal session.
type Session struct {
	URL       string
	TLS       *tls.Config
	SessionID string
	Ticket    string
	Cols      int
	Rows      int
	Log       *slog.Logger

	shell *Shell
	conn  *websocket.Conn
}

// Open spawns the shell, dials the terminal socket with the pinned CA, and
// writes TerminalHello as the first text frame. Only after Open succeeds may
// the agent reply ok to terminal_open. On error nothing is left running.
func (s *Session) Open(ctx context.Context) error {
	sh, err := StartShell(s.Cols, s.Rows)
	if err != nil {
		return err
	}

	dctx, dcancel := context.WithTimeout(ctx, 15*time.Second)
	defer dcancel()
	// No device token on this socket: the agent ticket is the only credential.
	conn, _, err := websocket.Dial(dctx, s.URL, &websocket.DialOptions{
		HTTPClient: &http.Client{
			Transport: &http.Transport{TLSClientConfig: s.TLS},
		},
	})
	if err != nil {
		sh.Close()
		return fmt.Errorf("terminal: dial: %w", err)
	}
	// Match the server's limit; coder/websocket defaults to 32 KiB, and an
	// oversized read would close the socket and kill the shell.
	conn.SetReadLimit(proto.TerminalMaxFrame)

	hello, _ := json.Marshal(proto.TerminalHello{
		SessionID:   s.SessionID,
		AgentTicket: s.Ticket,
	})
	if err := conn.Write(dctx, websocket.MessageText, hello); err != nil {
		conn.CloseNow()
		sh.Close()
		return fmt.Errorf("terminal: write hello: %w", err)
	}
	s.shell, s.conn = sh, conn
	return nil
}

// Serve relays PTY bytes and control frames until the shell exits, the socket
// closes, or ctx is cancelled, then tears everything down. When the shell
// exits on its own an exit control frame carries its code.
func (s *Session) Serve(ctx context.Context) {
	sh, conn := s.shell, s.conn
	defer conn.CloseNow()
	defer sh.Close()

	ptyDone := make(chan struct{})
	sockDone := make(chan struct{})

	// PTY -> WebSocket
	go func() {
		defer close(ptyDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := sh.PTY.Read(buf)
			if n > 0 {
				wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
				werr := conn.Write(wctx, websocket.MessageBinary, buf[:n])
				wcancel()
				if werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// WebSocket -> PTY
	go func() {
		defer close(sockDone)
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			switch typ {
			case websocket.MessageBinary:
				if _, err := sh.PTY.Write(data); err != nil {
					return
				}
			case websocket.MessageText:
				var ctrl proto.TerminalControl
				if err := json.Unmarshal(data, &ctrl); err != nil {
					continue
				}
				if ctrl.Type == "resize" {
					_ = sh.Resize(ctrl.Cols, ctrl.Rows)
				}
			}
		}
	}()

	select {
	case <-sh.Done():
	case <-sockDone:
		// Browser tab closed (or the server ended the session): kill the shell.
		sh.Close()
		s.log().Info("terminal socket closed; shell killed", "session", s.SessionID)
		return
	case <-ptyDone:
		// The PTY hit EOF because the shell is exiting, or the socket write
		// failed. Tell the two apart by whether the shell exits promptly.
		select {
		case <-sh.Done():
		case <-sockDone:
			sh.Close()
			s.log().Info("terminal socket closed; shell killed", "session", s.SessionID)
			return
		case <-time.After(time.Second):
			sh.Close()
			s.log().Info("terminal relay ended; shell killed", "session", s.SessionID)
			return
		}
	}
	// Let trailing output drain; a background job holding the PTY open
	// would keep the reader alive forever, so bound the wait.
	select {
	case <-ptyDone:
	case <-time.After(500 * time.Millisecond):
	}
	sh.Close()
	<-ptyDone
	exitMsg, _ := json.Marshal(proto.TerminalControl{Type: "exit", Code: sh.ExitCode()})
	ectx, ecancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = conn.Write(ectx, websocket.MessageText, exitMsg)
	ecancel()
	conn.Close(websocket.StatusNormalClosure, "shell exited")
	s.log().Info("terminal session ended", "session", s.SessionID, "code", sh.ExitCode())
}

func (s *Session) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// SelfTest spawns a shell through the production path, round-trips a marker
// through the PTY, resizes it, exits, and tears it down.
func SelfTest(timeout time.Duration) error {
	sh, err := StartShell(80, 24)
	if err != nil {
		return err
	}
	defer sh.Close()
	if err := sh.Resize(100, 30); err != nil {
		return fmt.Errorf("terminal: resize: %w", err)
	}

	found := make(chan struct{})
	go func() {
		var seen []byte
		buf := make([]byte, 4096)
		signalled := false
		for {
			n, err := sh.PTY.Read(buf)
			seen = append(seen, buf[:n]...)
			// The echoed input reads "KB_%s"; only the command output has the
			// joined marker.
			if !signalled && strings.Contains(string(seen), "KB_SELFTEST_OK") {
				close(found)
				signalled = true
			}
			if err != nil {
				return
			}
		}
	}()

	if _, err := sh.PTY.Write([]byte("printf 'KB_%s\\n' SELFTEST_OK; exit 0\n")); err != nil {
		return fmt.Errorf("terminal: write pty: %w", err)
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-found:
	case <-timer.C:
		return errors.New("terminal: selftest: no echo from shell")
	}
	select {
	case <-sh.Done():
	case <-timer.C:
		return errors.New("terminal: selftest: shell did not exit")
	}
	if code := sh.ExitCode(); code != 0 {
		return fmt.Errorf("terminal: selftest: shell exited %d", code)
	}
	return nil
}
