//go:build windows

package commands

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/jhyoong/KumaBoard/agent/config"
	"github.com/jhyoong/KumaBoard/proto"
)

func killZero(pid int) error { return nil }

func TestCheckFileRejectsOwnTempDir(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.ps1")
	if err := os.WriteFile(p, []byte("exit 0\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := CheckFile(p); err == nil {
		t.Fatalf("a script in the test's own temp dir passed: %s", got)
	}
}

func TestCheckFileShape(t *testing.T) {
	for path, want := range map[string]string{
		`scripts\s.ps1`:                     "not an absolute path",
		`\\server\share\s.ps1`:              "network share",
		`C:\kuma-no-such-dir\missing.ps1`:   "does not exist",
		filepath.Dir(os.Getenv("COMSPEC")):  "",
		filepath.Join(os.TempDir(), "nope"): "",
	} {
		_, err := CheckFile(path)
		if err == nil {
			t.Errorf("%s: passed", path)
		} else if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %q does not contain %q", path, err, want)
		}
	}
}

// A standard account cannot modify the system shell or anything on the way
// to it. An elevated one can create files in System32, so this only holds
// for the account the service really runs as.
func TestCheckFileSystemBinary(t *testing.T) {
	comspec := os.Getenv("COMSPEC")
	if comspec == "" {
		t.Skip("COMSPEC not set")
	}
	got, err := CheckFile(comspec)
	if err != nil {
		t.Skipf("%s does not pass for this account (elevated?): %v", comspec, err)
	}
	if !strings.EqualFold(filepath.Base(got), filepath.Base(comspec)) {
		t.Fatalf("resolved %q", got)
	}
}

// The job must end a child the command started, which killing only the
// command's own process does not.
func TestCancelKillsJobObjectTree(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := "$p = Start-Process ping.exe -ArgumentList '-n','120','127.0.0.1' -PassThru -WindowStyle Hidden; " +
		"Set-Content -Encoding ascii -Path child.pid -Value $p.Id; Wait-Process -Id $p.Id"
	r := New(map[string]config.Command{
		"slow": {Run: []string{"powershell.exe", "-NoProfile", "-Command", script}, TimeoutS: 120},
	}, dir, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	done := make(chan proto.CommandResult, 1)
	go r.Execute(context.Background(), "r1", "slow", func(res proto.CommandResult) error {
		done <- res
		return nil
	}, nil)

	var pid int
	deadline := time.Now().Add(30 * time.Second)
	for pid == 0 {
		if time.Now().After(deadline) {
			t.Fatal("child never started")
		}
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		time.Sleep(50 * time.Millisecond)
	}
	child, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatalf("open child %d: %v", pid, err)
	}
	defer windows.CloseHandle(child)

	if !r.Cancel("r1") {
		t.Fatal("cancel not delivered")
	}
	select {
	case res := <-done:
		if res.Status != proto.RunCancelled {
			t.Fatalf("result: %+v", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not end after cancel")
	}
	if ev, err := windows.WaitForSingleObject(child, 5000); err != nil || ev != windows.WAIT_OBJECT_0 {
		t.Fatalf("child %d still running after cancel: event %d err %v", pid, ev, err)
	}
}
