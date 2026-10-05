package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jhyoong/KumaBoard/proto"
)

func openTestDB(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestIngestAndListReleases(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()
	err := st.IngestRelease(ctx, "0.4.0", "linux", "amd64", "abc123", "sig1", 1000)
	if err != nil {
		t.Fatal(err)
	}
	err = st.IngestRelease(ctx, "0.4.0", "darwin", "arm64", "def456", "sig2", 2000)
	if err != nil {
		t.Fatal(err)
	}
	all, err := st.ListReleases(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d releases", len(all))
	}
	filtered, err := st.ListReleases(ctx, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].Version != "0.4.0" {
		t.Fatalf("filtered = %+v", filtered)
	}
}

func TestGetRelease(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()
	st.IngestRelease(ctx, "0.4.0", "linux", "amd64", "abc", "sig", 100)
	r, err := st.GetRelease(ctx, "0.4.0", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if r.SHA256 != "abc" {
		t.Fatalf("sha256 = %q", r.SHA256)
	}
}

func TestCreateAndUpdateUpgrade(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()
	st.CreateDevice(ctx, "test-dev", "", false, Schedule{})
	d, _ := st.GetDevice(ctx, "test-dev")
	id, err := st.CreateUpgrade(ctx, d.ID, "0.3.0", "0.4.0", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateUpgradeState(ctx, id, proto.UpgradeDownloading, ""); err != nil {
		t.Fatal(err)
	}
	u, err := st.GetLatestUpgrade(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.State != proto.UpgradeDownloading {
		t.Fatalf("state = %q", u.State)
	}
}

func TestGetActiveUpgrade(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()
	st.CreateDevice(ctx, "dev-a", "", false, Schedule{})
	d, _ := st.GetDevice(ctx, "dev-a")
	id, _ := st.CreateUpgrade(ctx, d.ID, "0.3.0", "0.4.0", "admin")
	active, err := st.GetActiveUpgrade(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if active == nil || active.ID != id {
		t.Fatal("expected active upgrade")
	}
	st.UpdateUpgradeState(ctx, id, proto.UpgradeVerified, "")
	active, _ = st.GetActiveUpgrade(ctx)
	if active != nil {
		t.Fatal("expected no active upgrade after verified")
	}
}

func TestHasFailedUpgrade(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()
	st.CreateDevice(ctx, "dev-b", "", false, Schedule{})
	d, _ := st.GetDevice(ctx, "dev-b")
	id, _ := st.CreateUpgrade(ctx, d.ID, "0.3.0", "0.4.0", "admin")
	st.UpdateUpgradeState(ctx, id, proto.UpgradeFailed, "verify_failed")
	if !st.HasFailedUpgrade(ctx, d.ID, "0.4.0") {
		t.Fatal("expected failed upgrade")
	}
	if st.HasFailedUpgrade(ctx, d.ID, "0.5.0") {
		t.Fatal("unexpected failed upgrade for different version")
	}
}

func TestAbandonUpgrade(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()
	st.CreateDevice(ctx, "dev-c", "", false, Schedule{})
	d, _ := st.GetDevice(ctx, "dev-c")
	id, _ := st.CreateUpgrade(ctx, d.ID, "0.3.0", "0.4.0", "admin")
	if err := st.AbandonUpgrade(ctx, id); err != nil {
		t.Fatal(err)
	}
	u, _ := st.GetLatestUpgrade(ctx, d.ID)
	if u.State != proto.UpgradeFailed || u.FailureReason != "abandoned" {
		t.Fatalf("state=%q reason=%q", u.State, u.FailureReason)
	}
}
