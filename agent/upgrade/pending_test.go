package upgrade

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPendingWriteRead(t *testing.T) {
	dir := t.TempDir()
	p := &Pending{
		FromVersion:    "0.3.0",
		ToVersion:      "0.4.0",
		StartedAt:      time.Now().UTC().Truncate(time.Second),
		RollbackReason: "",
	}
	path := filepath.Join(dir, PendingFile)
	if err := WritePending(path, p); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPending(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.FromVersion != p.FromVersion || got.ToVersion != p.ToVersion {
		t.Fatalf("mismatch: %+v vs %+v", got, p)
	}
	if got.StartedAt.Unix() != p.StartedAt.Unix() {
		t.Fatalf("time mismatch")
	}
}

func TestPendingReadMissing(t *testing.T) {
	got, err := ReadPending(filepath.Join(t.TempDir(), PendingFile))
	if err != nil {
		t.Fatal("expected nil error for missing file")
	}
	if got != nil {
		t.Fatal("expected nil pending for missing file")
	}
}

func TestPendingSetReason(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PendingFile)
	p := &Pending{FromVersion: "0.3.0", ToVersion: "0.4.0", StartedAt: time.Now().UTC()}
	WritePending(path, p)
	if err := SetRollbackReason(path, "no_handshake"); err != nil {
		t.Fatal(err)
	}
	got, _ := ReadPending(path)
	if got.RollbackReason != "no_handshake" {
		t.Fatalf("reason = %q", got.RollbackReason)
	}
}

func TestPendingDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PendingFile)
	WritePending(path, &Pending{FromVersion: "a", ToVersion: "b", StartedAt: time.Now().UTC()})
	os.Remove(path)
	got, _ := ReadPending(path)
	if got != nil {
		t.Fatal("expected nil after delete")
	}
}

func TestCheckStartupNormal(t *testing.T) {
	dir := t.TempDir()
	result := CheckStartup(dir, slog.Default())
	if result.State != StateNormal {
		t.Fatalf("expected normal, got %d", result.State)
	}
}

func TestCheckStartupStaleMarker(t *testing.T) {
	dir := t.TempDir()
	WritePending(filepath.Join(dir, PendingFile), &Pending{
		FromVersion: "0.1.0", ToVersion: "0.2.0", StartedAt: time.Now().UTC(),
	})
	result := CheckStartup(dir, slog.Default())
	if result.State != StateNormal {
		t.Fatalf("expected stale marker removed, got %d", result.State)
	}
	if _, err := os.Stat(filepath.Join(dir, PendingFile)); err == nil {
		t.Fatal("stale marker not deleted")
	}
}
