package commands

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	r.Execute(context.Background(), name, func(res proto.CommandResult) error {
		out = append(out, res)
		return nil
	})
	return out
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
