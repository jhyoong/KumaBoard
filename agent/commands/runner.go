// Package commands runs named commands declared in the agent config.
package commands

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/jhyoong/KumaBoard/agent/config"
	"github.com/jhyoong/KumaBoard/proto"
)

// Runner executes named commands with busy-locking, timeout, and output
// capture. Each command name can run at most once at a time.
type Runner struct {
	cmds    map[string]config.Command
	workDir string
	log     *slog.Logger

	// DispatchDelay is how long to wait after sending RunDispatched before
	// starting an expect_disconnect command. Tests lower this.
	DispatchDelay time.Duration

	mu      sync.Mutex
	running map[string]bool
}

// New creates a Runner.
func New(cmds map[string]config.Command, workDir string, log *slog.Logger) *Runner {
	return &Runner{
		cmds:          cmds,
		workDir:       workDir,
		log:           log,
		DispatchDelay: 2 * time.Second,
		running:       map[string]bool{},
	}
}

// Execute runs the named command and calls emit with the result. For
// expect_disconnect commands, emit is called with RunDispatched before
// execution, and the actual result is only logged.
func (r *Runner) Execute(ctx context.Context, name string, emit func(proto.CommandResult) error) {
	cmd, ok := r.cmds[name]
	if !ok {
		emit(proto.CommandResult{Status: proto.RunUnknownCommand})
		return
	}
	if !r.acquire(name) {
		emit(proto.CommandResult{Status: proto.RunBusy})
		return
	}
	defer r.release(name)

	if cmd.ExpectDisconnect {
		if err := emit(proto.CommandResult{Status: proto.RunDispatched}); err != nil {
			r.log.Warn("could not report dispatched; not running", "command", name, "err", err)
			return
		}
		time.Sleep(r.DispatchDelay)
		res := r.run(ctx, cmd)
		r.log.Info("expect_disconnect command finished", "command", name, "status", res.Status, "exit", res.ExitCode)
		return
	}

	res := r.run(ctx, cmd)
	if err := emit(res); err != nil {
		r.log.Warn("result discarded; session gone", "command", name, "status", res.Status)
	}
}

func (r *Runner) acquire(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running[name] {
		return false
	}
	r.running[name] = true
	return true
}

func (r *Runner) release(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.running, name)
}

func (r *Runner) run(ctx context.Context, cmd config.Command) proto.CommandResult {
	tctx, cancel := context.WithTimeout(ctx, time.Duration(cmd.TimeoutS)*time.Second)
	defer cancel()

	c := exec.CommandContext(tctx, cmd.Run[0], cmd.Run[1:]...)
	c.Dir = r.workDir
	c.Env = fixedEnv()

	stdout, stderr := &capWriter{}, &capWriter{}
	c.Stdout, c.Stderr = stdout, stderr
	c.WaitDelay = 2 * time.Second
	setProcAttr(c)

	start := time.Now()
	err := c.Run()

	res := proto.CommandResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		Truncated:  stdout.truncated || stderr.truncated,
		DurationMS: time.Since(start).Milliseconds(),
	}

	var exitErr *exec.ExitError
	switch {
	case errors.Is(tctx.Err(), context.DeadlineExceeded):
		res.Status, res.ExitCode = proto.RunTimeout, -1
	case err == nil:
		res.Status = proto.RunOK
	case errors.As(err, &exitErr):
		res.Status, res.ExitCode = proto.RunFailed, exitErr.ExitCode()
	default:
		res.Status, res.ExitCode = proto.RunFailed, -1
		res.Stderr += err.Error()
	}
	return res
}
