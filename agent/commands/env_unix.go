//go:build !windows

package commands

import (
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"
)

func fixedEnv() []string {
	return []string{
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + os.Getenv("HOME"),
		"LANG=C.UTF-8",
	}
}

// procTree is a command and everything it spawned: on Unix, its process
// group.
type procTree struct {
	pid atomic.Int64 // 0 until started; read from the timeout goroutine
}

// newProcTree prepares c, before it starts, to lead its own process group.
func newProcTree(c *exec.Cmd) (*procTree, error) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &procTree{}, nil
}

func (t *procTree) started(p *os.Process) error {
	t.pid.Store(int64(p.Pid))
	return nil
}

// terminate asks the whole group to exit.
func (t *procTree) terminate() error { return t.signal(syscall.SIGTERM) }

// kill ends the whole group at once.
func (t *procTree) kill() error { return t.signal(syscall.SIGKILL) }

func (t *procTree) signal(sig syscall.Signal) error {
	pid := int(t.pid.Load())
	if pid <= 0 {
		return os.ErrProcessDone
	}
	return syscall.Kill(-pid, sig)
}

// sweep waits until the group is empty or deadline passes, then kills
// whatever is still in it.
func (t *procTree) sweep(deadline time.Time) {
	for t.signal(syscall.Signal(0)) == nil {
		if !time.Now().Before(deadline) {
			t.kill()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (t *procTree) close() {}
