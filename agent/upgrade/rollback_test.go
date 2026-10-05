package upgrade

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
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
