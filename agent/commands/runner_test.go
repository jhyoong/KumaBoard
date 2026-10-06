package commands

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/agent/config"
	"github.com/jhyoong/KumaBoard/proto"
)

func newRunner(t *testing.T, cmds map[string]config.Command) *Runner {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin utilities")
	}
	r := New(cmds, t.TempDir(), slog.New(slog.NewTextHandler(os.Stderr, nil)))
	r.DispatchDelay = 10 * time.Millisecond
	return r
}

func collect(r *Runner, name string) []proto.CommandResult {
	var out []proto.CommandResult
	r.Execute(context.Background(), "run-"+name, name, func(res proto.CommandResult) error {
		out = append(out, res)
		return nil
	}, nil)
	return out
}

// stream runs name as runID and returns its result and every output chunk.
func stream(r *Runner, runID, name string) (proto.CommandResult, []proto.CommandOutput) {
	var res proto.CommandResult
	var mu sync.Mutex
	var chunks []proto.CommandOutput
	r.Execute(context.Background(), runID, name, func(got proto.CommandResult) error {
		res = got
		return nil
	}, func(o proto.CommandOutput) {
		mu.Lock()
		defer mu.Unlock()
		chunks = append(chunks, o)
	})
	return res, chunks
}

func joined(chunks []proto.CommandOutput, name string) string {
	var b strings.Builder
	for _, c := range chunks {
		if c.Stream == name {
			b.WriteString(c.Data)
		}
	}
	return b.String()
}

func TestOKAndFailed(t *testing.T) {
	r := newRunner(t, map[string]config.Command{
		"echo": {Run: []string{"/bin/echo", "hi"}, TimeoutS: 5},
		"fail": {Run: []string{"/bin/sh", "-c", "echo oops >&2; exit 3"}, TimeoutS: 5},
	})
	res := collect(r, "echo")
	if len(res) != 1 || res[0].Status != proto.RunOK || res[0].Stdout != "hi\n" || res[0].ExitCode != 0 {
		t.Fatalf("echo: %+v", res)
	}
	res = collect(r, "fail")
	if res[0].Status != proto.RunFailed || res[0].ExitCode != 3 || !strings.Contains(res[0].Stderr, "oops") {
		t.Fatalf("fail: %+v", res)
	}
}

func TestUnknownAndMissingBinary(t *testing.T) {
	r := newRunner(t, map[string]config.Command{
		"gone": {Run: []string{"/nonexistent/bin"}, TimeoutS: 5},
	})
	if res := collect(r, "nope"); res[0].Status != proto.RunUnknownCommand {
		t.Fatalf("unknown: %+v", res)
	}
	// An absolute run[0] that is not there fails the file check before exec.
	if res := collect(r, "gone"); res[0].Status != proto.RunRefused || !strings.Contains(res[0].Stderr, "/nonexistent") {
		t.Fatalf("missing binary: %+v", res)
	}
}

func TestBareNameNotFound(t *testing.T) {
	r := newRunner(t, map[string]config.Command{
		"gone": {Run: []string{"kuma-no-such-binary"}, TimeoutS: 5},
	})
	if res := collect(r, "gone"); res[0].Status != proto.RunFailed || res[0].ExitCode != -1 {
		t.Fatalf("missing binary: %+v", res)
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	r := newRunner(t, map[string]config.Command{
		"slow": {Run: []string{"/bin/sh", "-c", "sleep 30 & wait"}, TimeoutS: 1},
	})
	start := time.Now()
	res := collect(r, "slow")
	if res[0].Status != proto.RunTimeout {
		t.Fatalf("timeout: %+v", res)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("timeout did not kill the process group promptly")
	}
}

func TestBusy(t *testing.T) {
	r := newRunner(t, map[string]config.Command{
		"slow": {Run: []string{"/bin/sleep", "1"}, TimeoutS: 5},
	})
	done := make(chan struct{})
	go func() {
		collect(r, "slow")
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	if res := collect(r, "slow"); res[0].Status != proto.RunBusy {
		t.Fatalf("busy: %+v", res)
	}
	<-done
	if res := collect(r, "slow"); res[0].Status != proto.RunOK {
		t.Fatalf("after release: %+v", res)
	}
}

func TestTruncation(t *testing.T) {
	r := newRunner(t, map[string]config.Command{
		"big": {Run: []string{"/bin/sh", "-c", "head -c 100000 /dev/zero | tr '\\0' a"}, TimeoutS: 5},
	})
	res := collect(r, "big")
	if !res[0].Truncated || len(res[0].Stdout) != MaxOutput {
		t.Fatalf("truncation: truncated=%v len=%d", res[0].Truncated, len(res[0].Stdout))
	}
}

// The cap keeps the newest output, like tail, not the oldest.
func TestTruncationKeepsTail(t *testing.T) {
	r := newRunner(t, map[string]config.Command{
		"lines": {Run: []string{"/bin/sh", "-c", "i=0; while [ $i -lt 20000 ]; do echo line-$i; i=$((i+1)); done"}, TimeoutS: 20},
	})
	res := collect(r, "lines")[0]
	if res.Status != proto.RunOK || !res.Truncated {
		t.Fatalf("status %s truncated %v", res.Status, res.Truncated)
	}
	if len(res.Stdout) != MaxOutput || !strings.HasSuffix(res.Stdout, "line-19999\n") || strings.Contains(res.Stdout, "line-0\n") {
		t.Fatalf("not the tail: len=%d end=%q", len(res.Stdout), res.Stdout[len(res.Stdout)-30:])
	}
}

func TestStreamsOutputBeforeResult(t *testing.T) {
	r := newRunner(t, map[string]config.Command{
		"slow": {Run: []string{"/bin/sh", "-c", "for i in 1 2 3 4; do echo out-$i; echo err-$i >&2; sleep 0.15; done"}, TimeoutS: 10},
	})
	r.FlushInterval = 50 * time.Millisecond
	res, chunks := stream(r, "r1", "slow")
	if res.Status != proto.RunOK {
		t.Fatalf("result: %+v", res)
	}
	if len(chunks) < 4 {
		t.Fatalf("expected several chunks while running, got %d: %+v", len(chunks), chunks)
	}
	for i, c := range chunks {
		if c.Seq != i+1 || c.Skipped {
			t.Fatalf("chunk %d: %+v", i, c)
		}
	}
	if got := joined(chunks, proto.StreamStdout); got != res.Stdout || got != "out-1\nout-2\nout-3\nout-4\n" {
		t.Fatalf("stdout chunks %q, result %q", got, res.Stdout)
	}
	if got := joined(chunks, proto.StreamStderr); got != res.Stderr {
		t.Fatalf("stderr chunks %q, result %q", got, res.Stderr)
	}
}

func TestBurstIsSkippedNotBuffered(t *testing.T) {
	r := newRunner(t, map[string]config.Command{
		"burst": {Run: []string{"/bin/sh", "-c", "head -c 300000 /dev/zero | tr '\\0' a; echo; echo end"}, TimeoutS: 10},
	})
	r.FlushInterval = 5 * time.Second // one flush, at the end
	res, chunks := stream(r, "r1", "burst")
	if res.Status != proto.RunOK || !res.Truncated {
		t.Fatalf("result: status %s truncated %v", res.Status, res.Truncated)
	}
	if len(chunks) != 1 || !chunks[0].Skipped || len(chunks[0].Data) > MaxOutput || !strings.HasSuffix(chunks[0].Data, "\nend\n") {
		t.Fatalf("chunks: %d, first skipped=%v len=%d", len(chunks), chunks[0].Skipped, len(chunks[0].Data))
	}
}

func TestExpectDisconnectDoesNotStream(t *testing.T) {
	r := newRunner(t, map[string]config.Command{
		"bye": {Run: []string{"/bin/echo", "hi"}, TimeoutS: 5, ExpectDisconnect: true},
	})
	res, chunks := stream(r, "r1", "bye")
	if res.Status != proto.RunDispatched || len(chunks) != 0 {
		t.Fatalf("result %+v chunks %+v", res, chunks)
	}
}

func TestCancelKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	r := newRunner(t, map[string]config.Command{
		"slow": {Run: []string{"/bin/sh", "-c", "sleep 30 & echo $! > " + pidFile + "; echo started; wait"}, TimeoutS: 60},
	})
	r.FlushInterval = 20 * time.Millisecond
	if r.Cancel("r1") {
		t.Fatal("cancel of a run that has not started must be ignored")
	}
	done := make(chan proto.CommandResult, 1)
	go func() {
		res, _ := stream(r, "r1", "slow")
		done <- res
	}()
	waitForFile(t, pidFile)
	if r.Cancel("other") {
		t.Fatal("unknown run ID must be ignored")
	}
	start := time.Now()
	if !r.Cancel("r1") {
		t.Fatal("cancel not delivered")
	}
	res := <-done
	if res.Status != proto.RunCancelled || !strings.Contains(res.Stdout, "started") {
		t.Fatalf("result: %+v", res)
	}
	if d := time.Since(start); d > r.KillGrace {
		t.Fatalf("SIGTERM should have ended it well inside the grace period, took %s", d)
	}
	if childAlive(t, pidFile) {
		t.Fatal("child process survived the cancel")
	}
	if r.Cancel("r1") {
		t.Fatal("cancel after the run finished must be ignored")
	}
}

func TestCancelEscalatesToKill(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	// Both the shell and its child ignore SIGTERM.
	r := newRunner(t, map[string]config.Command{
		"stubborn": {Run: []string{"/bin/sh", "-c", "trap '' TERM; sleep 30 & echo $! > " + pidFile + "; wait"}, TimeoutS: 60},
	})
	r.KillGrace = 300 * time.Millisecond
	done := make(chan proto.CommandResult, 1)
	go func() {
		res, _ := stream(r, "r1", "stubborn")
		done <- res
	}()
	waitForFile(t, pidFile)
	start := time.Now()
	r.Cancel("r1")
	res := <-done
	if res.Status != proto.RunCancelled {
		t.Fatalf("result: %+v", res)
	}
	if d := time.Since(start); d < r.KillGrace || d > 5*time.Second {
		t.Fatalf("expected a kill after the %s grace, took %s", r.KillGrace, d)
	}
	if childAlive(t, pidFile) {
		t.Fatal("child process survived the kill")
	}
}

func TestSetCommandsSwapsWithoutTouchingARunInFlight(t *testing.T) {
	r := newRunner(t, map[string]config.Command{
		"nap": {Run: []string{"/bin/sh", "-c", "sleep 0.3; echo old"}, TimeoutS: 5},
	})
	done := make(chan []proto.CommandResult, 1)
	go func() { done <- collect(r, "nap") }()
	time.Sleep(100 * time.Millisecond)
	r.SetCommands(map[string]config.Command{
		"echo": {Run: []string{"/bin/echo", "new"}, TimeoutS: 5},
	})
	if res := collect(r, "echo"); res[0].Status != proto.RunOK || res[0].Stdout != "new\n" {
		t.Fatalf("new command: %+v", res)
	}
	if res := <-done; res[0].Status != proto.RunOK || res[0].Stdout != "old\n" {
		t.Fatalf("removed command's run was disturbed: %+v", res)
	}
	if res := collect(r, "nap"); res[0].Status != proto.RunUnknownCommand {
		t.Fatalf("removed command still runs: %+v", res)
	}
	defs, problems := r.Declared()
	if len(defs) != 1 || defs[0].Name != "echo" || len(problems) != 0 {
		t.Fatalf("declared %+v problems %+v", defs, problems)
	}
}

func TestRefusedWhenCheckFails(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "s.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The real check: a script in the test's own temp dir is writable by
	// the account running it.
	r := newRunner(t, map[string]config.Command{
		"mine": {Script: script, TimeoutS: 5},
		"via":  {Run: []string{"/bin/sh"}, Script: script, TimeoutS: 5},
		"ok":   {Run: []string{"/bin/echo", "hi"}, TimeoutS: 5},
	})
	defs, problems := r.Declared()
	if len(defs) != 1 || defs[0].Name != "ok" {
		t.Fatalf("declared: %+v", defs)
	}
	if len(problems) != 2 || problems[0].Name != "mine" || problems[1].Name != "via" || problems[0].Reason == "" {
		t.Fatalf("problems: %+v", problems)
	}
	for _, name := range []string{"mine", "via"} {
		res := collect(r, name)
		if len(res) != 1 || res[0].Status != proto.RunRefused || !strings.Contains(res[0].Stderr, "refused: ") {
			t.Fatalf("%s: %+v", name, res)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a refused script ran")
	}
}

func TestRunsResolvedScript(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.sh")
	if err := os.WriteFile(real, []byte("#!/bin/sh\necho \"$0 $#\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.sh")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	r := newRunner(t, map[string]config.Command{
		"direct": {Script: link, TimeoutS: 5},
		"via":    {Run: []string{"/bin/sh"}, Script: link, TimeoutS: 5},
	})
	// Stand in for the permission check, which a temp dir can never pass,
	// but keep its contract: return the resolved path.
	var checked []string
	r.SetCheck(func(p string) (string, error) {
		checked = append(checked, p)
		return filepath.EvalSymlinks(p)
	})
	want, _ := filepath.EvalSymlinks(real)
	for _, name := range []string{"direct", "via"} {
		res := collect(r, name)
		if res[0].Status != proto.RunOK || res[0].Stdout != want+" 0\n" {
			t.Fatalf("%s: %+v", name, res)
		}
	}
	if !slices.Equal(checked, []string{link, link, "/bin/sh"}) {
		t.Fatalf("checked %v", checked)
	}
}

func TestExpectDisconnect(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	r := newRunner(t, map[string]config.Command{
		"bye": {Run: []string{"/usr/bin/touch", marker}, TimeoutS: 5, ExpectDisconnect: true},
	})
	res := collect(r, "bye")
	if len(res) != 1 || res[0].Status != proto.RunDispatched {
		t.Fatalf("expect exactly one dispatched result: %+v", res)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("command did not run after dispatch")
	}
}

func TestFixedEnvironment(t *testing.T) {
	r := newRunner(t, map[string]config.Command{
		"env": {Run: []string{"/usr/bin/env"}, TimeoutS: 5},
	})
	t.Setenv("KUMA_LEAK", "1")
	res := collect(r, "env")
	if strings.Contains(res[0].Stdout, "KUMA_LEAK") || !strings.Contains(res[0].Stdout, "PATH=") {
		t.Fatalf("environment not fixed: %s", res[0].Stdout)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

// childAlive reports whether the process whose PID is in pidFile still runs,
// allowing a moment for a killed one to be reaped.
func childAlive(t *testing.T, pidFile string) bool {
	t.Helper()
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if killZero(pid) != nil {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}
