// Package commands runs named commands declared in the agent config.
package commands

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jhyoong/KumaBoard/agent/config"
	"github.com/jhyoong/KumaBoard/proto"
)

// Runner executes named commands with busy-locking, timeout, cancel and
// output capture. Each command name can run at most once at a time; different
// names may overlap. Commands take no parameters: a run is started by name
// and stopped by run ID, and nothing from the server reaches argv.
type Runner struct {
	workDir string
	log     *slog.Logger

	// DispatchDelay is how long to wait after sending RunDispatched before
	// starting an expect_disconnect command. Tests lower this.
	DispatchDelay time.Duration
	// FlushInterval is how often buffered output is streamed while a command
	// runs.
	FlushInterval time.Duration
	// KillGrace is how long a cancelled command gets to exit after the polite
	// stop signal before its whole process tree is killed.
	KillGrace time.Duration

	mu sync.Mutex
	// check is the file permission check applied to each command's script
	// and absolute run[0]: CheckFile unless a test replaced it.
	check   func(path string) (resolved string, err error)
	cmds    map[string]config.Command
	running map[string]bool     // by command name
	live    map[string]*liveRun // by run ID
}

// liveRun is one run between Execute being called and returning.
type liveRun struct {
	mu        sync.Mutex
	cancelled bool
	stop      func() // set once the process has started
}

// New creates a Runner.
func New(cmds map[string]config.Command, workDir string, log *slog.Logger) *Runner {
	return &Runner{
		cmds:          cmds,
		workDir:       workDir,
		log:           log,
		DispatchDelay: 2 * time.Second,
		FlushInterval: 250 * time.Millisecond,
		KillGrace:     3 * time.Second,
		check:         CheckFile,
		running:       map[string]bool{},
		live:          map[string]*liveRun{},
	}
}

// SetCommands replaces the command set after a config reload. A run in
// flight keeps the definition it started with, and is not stopped if its
// command was removed.
func (r *Runner) SetCommands(cmds map[string]config.Command) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = cmds
}

// SetCheck replaces the file permission check. Useful in tests, where a
// script the test wrote can never pass the real one.
func (r *Runner) SetCheck(check func(path string) (resolved string, err error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.check = check
}

// Declared runs the file check over the current command set and returns what
// may be declared to the server and what is withheld, with reasons.
func (r *Runner) Declared() ([]proto.CommandDef, []proto.CommandProblem) {
	r.mu.Lock()
	cmds, check := r.cmds, r.check
	r.mu.Unlock()
	return Vet(cmds, check)
}

// Execute runs the named command as run runID and calls emit with the
// result. While it runs, output is passed to output in chunks if output is
// not nil. For expect_disconnect commands, emit is called with RunDispatched
// before execution, nothing is streamed, and the actual result is only logged.
func (r *Runner) Execute(ctx context.Context, runID, name string, emit func(proto.CommandResult) error, output func(proto.CommandOutput)) {
	r.mu.Lock()
	cmd, ok := r.cmds[name]
	r.mu.Unlock()
	if !ok {
		emit(proto.CommandResult{Status: proto.RunUnknownCommand})
		return
	}
	if !r.acquire(name) {
		emit(proto.CommandResult{Status: proto.RunBusy})
		return
	}
	defer r.release(name)
	lr := r.track(runID)
	defer r.untrack(runID, lr)

	if cmd.ExpectDisconnect {
		if err := emit(proto.CommandResult{Status: proto.RunDispatched}); err != nil {
			r.log.Warn("could not report dispatched; not running", "command", name, "err", err)
			return
		}
		time.Sleep(r.DispatchDelay)
		res := r.run(ctx, cmd, lr, nil)
		r.log.Info("expect_disconnect command finished", "command", name, "status", res.Status, "exit", res.ExitCode)
		return
	}

	res := r.run(ctx, cmd, lr, output)
	if err := emit(res); err != nil {
		r.log.Warn("result discarded; session gone", "command", name, "status", res.Status)
	}
}

// Cancel stops the run with the given ID: a polite stop signal to its
// process tree, then a kill after KillGrace. The run ends as RunCancelled.
// An unknown or already finished run ID is ignored and reported as false.
func (r *Runner) Cancel(runID string) bool {
	r.mu.Lock()
	lr := r.live[runID]
	r.mu.Unlock()
	if lr == nil {
		return false
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if !lr.cancelled {
		lr.cancelled = true
		if lr.stop != nil {
			lr.stop()
		}
	}
	return true
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

func (r *Runner) track(runID string) *liveRun {
	lr := &liveRun{}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.live[runID] = lr
	return lr
}

func (r *Runner) untrack(runID string, lr *liveRun) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live[runID] == lr {
		delete(r.live, runID)
	}
}

func (r *Runner) run(ctx context.Context, cmd config.Command, lr *liveRun, output func(proto.CommandOutput)) proto.CommandResult {
	// Checked again immediately before exec: the script may have become
	// writable since it was declared. What runs is the resolved path.
	r.mu.Lock()
	check := r.check
	r.mu.Unlock()
	argv, program, err := checkCommand(cmd, check)
	if err != nil {
		return proto.CommandResult{Status: proto.RunRefused, ExitCode: -1, Stderr: "refused: " + err.Error() + "\n"}
	}

	tctx, cancel := context.WithTimeout(ctx, time.Duration(cmd.TimeoutS)*time.Second)
	defer cancel()

	c := exec.CommandContext(tctx, argv[0], argv[1:]...)
	if program != "" {
		// Execute the resolved file but keep argv[0] as configured, for
		// programs that look at their own name.
		c.Path, c.Err = program, nil
	}
	c.Dir = r.workDir
	c.Env = fixedEnv()

	stdout, stderr := &capWriter{}, &capWriter{}
	c.Stdout, c.Stderr = stdout, stderr
	c.WaitDelay = 2 * time.Second
	tree, err := newProcTree(c)
	if err != nil {
		return proto.CommandResult{Status: proto.RunFailed, ExitCode: -1, Stderr: err.Error()}
	}
	defer tree.close()
	// On timeout the context kills the whole tree, not just the child.
	c.Cancel = tree.kill

	start := time.Now()
	var cancelled atomic.Bool
	err = c.Start()
	if err == nil {
		if aerr := tree.started(c.Process); aerr != nil {
			r.log.Warn("could not track process tree; children may outlive a cancel or timeout", "err", aerr)
		}
		// From here a cancel reaches the process. One that arrived while it
		// was starting is acted on now.
		var killAt time.Time
		stop := func() {
			cancelled.Store(true)
			killAt = time.Now().Add(r.KillGrace)
			tree.terminate()
			grace := time.AfterFunc(r.KillGrace, cancel)
			context.AfterFunc(tctx, func() { grace.Stop() })
		}
		lr.mu.Lock()
		lr.stop = stop
		if lr.cancelled {
			stop()
		}
		lr.mu.Unlock()

		flushed := make(chan struct{})
		stopFlush := make(chan struct{})
		go r.flush(stdout, stderr, output, stopFlush, flushed)
		err = c.Wait()
		close(stopFlush)
		<-flushed

		lr.mu.Lock()
		lr.stop = nil
		lr.mu.Unlock()
		if cancelled.Load() {
			// The command itself is gone, but something it started may
			// have ignored the stop signal: give it the rest of the grace
			// period, then kill what is left.
			tree.sweep(killAt)
		}
	}

	res := proto.CommandResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		Truncated:  stdout.truncated() || stderr.truncated(),
		DurationMS: time.Since(start).Milliseconds(),
	}

	var exitErr *exec.ExitError
	switch {
	case cancelled.Load():
		res.Status, res.ExitCode = proto.RunCancelled, -1
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

// flush streams what the command has written since the last tick, per
// stream, until stop is closed, then sends whatever is left. The ring caps a
// chunk at MaxOutput, which bounds the wire at MaxOutput per stream per tick
// however fast the command prints.
func (r *Runner) flush(stdout, stderr *capWriter, output func(proto.CommandOutput), stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	if output == nil {
		return
	}
	seq := 0
	send := func(final bool) {
		for _, s := range []struct {
			name string
			w    *capWriter
		}{{proto.StreamStdout, stdout}, {proto.StreamStderr, stderr}} {
			data, skipped := s.w.next(final)
			if data == "" && !skipped {
				continue
			}
			seq++
			output(proto.CommandOutput{Seq: seq, Stream: s.name, Data: data, Skipped: skipped})
		}
	}
	t := time.NewTicker(r.FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			send(false)
		case <-stop:
			send(true)
			return
		}
	}
}
