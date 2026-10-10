package upgrade

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

func TestPendingNewFieldsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), PendingFile)
	p := &Pending{
		FromVersion: "0.3.0", ToVersion: "0.4.0", StartedAt: time.Now().UTC().Truncate(time.Second),
		RollbackReason: "no_handshake", UpgradeID: "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		RollbackDetail: "no handshake within 2m0s\n12:00:03 dial: refused", Starts: 3,
	}
	if err := WritePending(path, p); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPending(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.StartedAt.Equal(p.StartedAt) {
		t.Fatalf("started_at = %v, want %v", got.StartedAt, p.StartedAt)
	}
	got.StartedAt = p.StartedAt
	if *got != *p {
		t.Fatalf("round trip: %+v, want %+v", got, p)
	}
}

// The marker is written by one binary and read by the other: a file from a
// binary without the new fields must decode, and so must one with fields
// this binary has never heard of.
func TestPendingOldAndNewerFormats(t *testing.T) {
	path := filepath.Join(t.TempDir(), PendingFile)
	old := `{"from_version":"0.2.4","to_version":"0.2.5","started_at":"2026-10-06T12:00:00Z","rollback_reason":""}`
	os.WriteFile(path, []byte(old), 0o644)
	got, err := ReadPending(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.FromVersion != "0.2.4" || got.ToVersion != "0.2.5" || got.StartedAt.IsZero() {
		t.Fatalf("old format: %+v", got)
	}
	if got.UpgradeID != "" || got.RollbackDetail != "" || got.Starts != 0 {
		t.Fatalf("old format grew values: %+v", got)
	}

	// A binary that does not set the new fields writes none of them.
	if err := WritePending(path, got); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	for _, key := range []string{"upgrade_id", "rollback_detail", "starts"} {
		if strings.Contains(string(b), key) {
			t.Errorf("empty %s was written: %s", key, b)
		}
	}

	newer := `{"from_version":"0.2.4","to_version":"0.2.5","started_at":"2026-10-06T12:00:00Z","rollback_reason":"x","upgrade_id":"U","from_the_future":{"a":1}}`
	os.WriteFile(path, []byte(newer), 0o644)
	if got, err := ReadPending(path); err != nil || got.UpgradeID != "U" {
		t.Fatalf("newer format: %+v, %v", got, err)
	}
}

func TestPendingSetRollback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PendingFile)
	p := &Pending{FromVersion: "0.3.0", ToVersion: "0.4.0", StartedAt: time.Now().UTC(), UpgradeID: "U", Starts: 2}
	WritePending(path, p)
	if err := SetRollback(path, "no_handshake", "why"); err != nil {
		t.Fatal(err)
	}
	got, _ := ReadPending(path)
	if got.RollbackReason != "no_handshake" || got.RollbackDetail != "why" {
		t.Fatalf("reason = %q, detail = %q", got.RollbackReason, got.RollbackDetail)
	}
	if got.UpgradeID != "U" || got.Starts != 2 || got.ToVersion != "0.4.0" {
		t.Fatalf("other fields lost: %+v", got)
	}
	if err := SetRollback(filepath.Join(dir, "absent.json"), "x", "y"); err == nil {
		t.Fatal("missing marker reported no error")
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

func TestWritePendingLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PendingFile)
	for _, v := range []string{"0.4.0", "0.5.0"} {
		if err := WritePending(path, &Pending{FromVersion: "0.3.0", ToVersion: v}); err != nil {
			t.Fatal(err)
		}
	}
	if p, err := ReadPending(path); err != nil || p.ToVersion != "0.5.0" {
		t.Fatalf("pending %+v, err %v", p, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("dir has %d entries, want 1", len(entries))
	}
}
