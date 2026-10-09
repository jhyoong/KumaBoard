package upgrade

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/internal/buildinfo"
	"github.com/jhyoong/KumaBoard/proto"
)

func TestAgentBinaryName(t *testing.T) {
	for goos, want := range map[string]string{
		"windows": "kuma-agent.exe",
		"linux":   "kuma-agent",
		"darwin":  "kuma-agent",
	} {
		if got := agentBinaryName(goos); got != want {
			t.Errorf("agentBinaryName(%q) = %q, want %q", goos, got, want)
		}
	}
}

func TestCheckStartupCleansStaleFailed(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, agentBinaryName(runtime.GOOS))
	failed := current + ".failed"
	unrelated := filepath.Join(dir, "config.yaml")
	os.WriteFile(current, []byte("current-binary"), 0o755)
	os.WriteFile(failed, []byte("failed-binary"), 0o755)
	os.WriteFile(unrelated, []byte("unrelated"), 0o644)

	result := CheckStartup(dir, slog.Default())
	if result.State != StateNormal {
		t.Fatalf("State = %d, want StateNormal", result.State)
	}

	// Only Windows RestoreOld produces .failed; CleanupFailed is a no-op elsewhere.
	if runtime.GOOS == "windows" {
		if _, err := os.Stat(failed); !os.IsNotExist(err) {
			t.Fatalf("stale %s should be removed, stat err = %v", filepath.Base(failed), err)
		}
	}
	if got, _ := os.ReadFile(current); string(got) != "current-binary" {
		t.Fatalf("current = %q", got)
	}
	if got, _ := os.ReadFile(unrelated); string(got) != "unrelated" {
		t.Fatalf("unrelated = %q", got)
	}
	entries, _ := os.ReadDir(dir)
	want := 3
	if runtime.GOOS == "windows" {
		want = 2
	}
	if len(entries) != want {
		t.Fatalf("dir has %d entries, want %d", len(entries), want)
	}
}

// stubExit replaces exit for the test and returns where the code lands (-1
// until exit is called).
func stubExit(t *testing.T) *int {
	t.Helper()
	code := -1
	old := exit
	exit = func(c int) { code = c }
	t.Cleanup(func() { exit = old })
	return &code
}

func setVersion(t *testing.T, v string) {
	t.Helper()
	old := buildinfo.Version
	buildinfo.Version = v
	t.Cleanup(func() { buildinfo.Version = old })
}

// swappedDir is a binary directory just after a swap from 0.3.0 to 0.4.0:
// the new binary, the old one beside it, and the marker.
func swappedDir(t *testing.T) (dir, bin, marker string) {
	t.Helper()
	dir = t.TempDir()
	bin = filepath.Join(dir, agentBinaryName(runtime.GOOS))
	marker = filepath.Join(dir, PendingFile)
	os.WriteFile(bin, []byte("new-binary"), 0o755)
	os.WriteFile(bin+".old", []byte("old-binary"), 0o755)
	if err := WritePending(marker, &Pending{
		FromVersion: "0.3.0", ToVersion: "0.4.0", StartedAt: time.Now().UTC(), UpgradeID: "01ARZ3NDEKTSV4RRFFQ69G5FAV",
	}); err != nil {
		t.Fatal(err)
	}
	return dir, bin, marker
}

func wantBinary(t *testing.T, bin, want string) {
	t.Helper()
	if got, _ := os.ReadFile(bin); string(got) != want {
		t.Fatalf("binary = %q, want %q", got, want)
	}
}

func TestCheckStartupCountsStartsAndTripsCrashLoop(t *testing.T) {
	setVersion(t, "0.4.0")
	code := stubExit(t)
	dir, bin, marker := swappedDir(t)

	for i := 1; i <= MaxProbationStarts; i++ {
		res := checkStartup(dir, bin, slog.Default())
		if res.State != StateProbation {
			t.Fatalf("start %d: State = %d, want StateProbation", i, res.State)
		}
		if res.UpgradeID != "01ARZ3NDEKTSV4RRFFQ69G5FAV" || res.PendingMarker != marker {
			t.Fatalf("start %d: result %+v", i, res)
		}
		if p, _ := ReadPending(marker); p.Starts != i {
			t.Fatalf("start %d: marker Starts = %d", i, p.Starts)
		}
	}
	if *code != -1 {
		t.Fatalf("exit(%d) before the limit", *code)
	}
	wantBinary(t, bin, "new-binary")

	checkStartup(dir, bin, slog.Default())
	if *code != 1 {
		t.Fatalf("exit code = %d, want 1", *code)
	}
	wantBinary(t, bin, "old-binary")
	p, err := ReadPending(marker)
	if err != nil || p == nil {
		t.Fatalf("marker gone after rollback: %v", err)
	}
	if p.RollbackReason != proto.UpgradeReasonCrashLoop {
		t.Fatalf("reason = %q, want crash_loop", p.RollbackReason)
	}
	if p.RollbackDetail != "started 6 times without completing a handshake" {
		t.Fatalf("detail = %q", p.RollbackDetail)
	}

	// The restored binary reads the cause back.
	setVersion(t, "0.3.0")
	res := checkStartup(dir, bin, slog.Default())
	if res.State != StateRolledBack || res.RollbackReason != proto.UpgradeReasonCrashLoop ||
		res.RollbackDetail != p.RollbackDetail || res.UpgradeID != p.UpgradeID {
		t.Fatalf("rolled-back result %+v", res)
	}
}

func TestRollbackIfProbation(t *testing.T) {
	log := slog.Default()

	t.Run("no marker", func(t *testing.T) {
		setVersion(t, "0.4.0")
		_, bin, marker := swappedDir(t)
		os.Remove(marker)
		if RollbackIfProbation(bin, proto.UpgradeReasonStartupFailed, "boom", log) {
			t.Fatal("rolled back without a marker")
		}
		wantBinary(t, bin, "new-binary")
	})

	t.Run("probation", func(t *testing.T) {
		setVersion(t, "0.4.0")
		_, bin, marker := swappedDir(t)
		if !RollbackIfProbation(bin, proto.UpgradeReasonStartupFailed, "config: parse: yaml: line 3", log) {
			t.Fatal("binary on probation was not rolled back")
		}
		wantBinary(t, bin, "old-binary")
		p, _ := ReadPending(marker)
		if p == nil || p.RollbackReason != proto.UpgradeReasonStartupFailed || p.RollbackDetail != "config: parse: yaml: line 3" {
			t.Fatalf("marker %+v", p)
		}
		if p.UpgradeID != "01ARZ3NDEKTSV4RRFFQ69G5FAV" {
			t.Fatalf("upgrade id lost: %+v", p)
		}
	})

	// The old binary is already back; its own startup failure is not an
	// upgrade failure and must not disturb the recorded cause.
	t.Run("rolled back", func(t *testing.T) {
		setVersion(t, "0.3.0")
		_, bin, marker := swappedDir(t)
		SetRollback(marker, proto.UpgradeReasonNoHandshake, "earlier")
		if RollbackIfProbation(bin, proto.UpgradeReasonStartupFailed, "boom", log) {
			t.Fatal("rolled back from the from_version binary")
		}
		wantBinary(t, bin, "new-binary")
		if p, _ := ReadPending(marker); p.RollbackReason != proto.UpgradeReasonNoHandshake || p.RollbackDetail != "earlier" {
			t.Fatalf("marker changed: %+v", p)
		}
	})

	t.Run("unrelated version", func(t *testing.T) {
		setVersion(t, "0.9.9")
		_, bin, _ := swappedDir(t)
		if RollbackIfProbation(bin, proto.UpgradeReasonStartupFailed, "boom", log) {
			t.Fatal("rolled back on a stale marker")
		}
		wantBinary(t, bin, "new-binary")
	})
}

func TestRollbackRecordsReasonAndDetail(t *testing.T) {
	code := stubExit(t)
	dir, bin, marker := swappedDir(t)
	long := "first line\n" + strings.Repeat("x", 2*proto.MaxUpgradeDetailLen) + "\nlast line"
	Rollback(dir, bin, marker, proto.UpgradeReasonNoHandshake, long, slog.Default())
	if *code != 1 {
		t.Fatalf("exit code = %d, want 1", *code)
	}
	wantBinary(t, bin, "old-binary")
	p, _ := ReadPending(marker)
	if p == nil || p.RollbackReason != proto.UpgradeReasonNoHandshake {
		t.Fatalf("marker %+v", p)
	}
	if len(p.RollbackDetail) > proto.MaxUpgradeDetailLen ||
		!strings.HasPrefix(p.RollbackDetail, "first line") || !strings.HasSuffix(p.RollbackDetail, "last line") {
		t.Fatalf("detail not bounded to head and tail: %d bytes", len(p.RollbackDetail))
	}
}

// The marker cannot be read or written (here: it is a directory). The old
// binary must come back regardless.
func TestRollbackSurvivesUnwritableMarker(t *testing.T) {
	code := stubExit(t)
	dir, bin, marker := swappedDir(t)
	os.Remove(marker)
	if err := os.Mkdir(marker, 0o755); err != nil {
		t.Fatal(err)
	}
	Rollback(dir, bin, marker, proto.UpgradeReasonNoHandshake, "why", slog.Default())
	if *code != 1 {
		t.Fatalf("exit code = %d, want 1", *code)
	}
	wantBinary(t, bin, "old-binary")

	// Same with the marker missing altogether.
	dir, bin, marker = swappedDir(t)
	os.Remove(marker)
	Rollback(dir, bin, marker, proto.UpgradeReasonNoHandshake, "why", slog.Default())
	wantBinary(t, bin, "old-binary")
}
