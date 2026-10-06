package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

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

// newUpgradeDevice registers a device and returns its id.
func newUpgradeDevice(t *testing.T, st *Store, name string) int64 {
	t.Helper()
	d, _, err := st.CreateDevice(context.Background(), name, "", false, Schedule{})
	if err != nil {
		t.Fatal(err)
	}
	return d.ID
}

// record is RecordUpgradeEvent that fails the test on an error.
func record(t *testing.T, st *Store, id, source, state, reason, detail string) bool {
	t.Helper()
	applied, err := st.RecordUpgradeEvent(context.Background(), UpgradeEvent{
		UpgradeID: id, Source: source, State: state, Reason: reason, Detail: detail})
	if err != nil {
		t.Fatalf("record %s/%s: %v", source, state, err)
	}
	return applied
}

func TestCreateUpgradeRecordsRequestedEvent(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()
	dev := newUpgradeDevice(t, st, "test-dev")
	id, err := st.CreateUpgrade(ctx, dev, "0.3.0", "0.4.0", "admin")
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.GetUpgrade(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if u.State != proto.UpgradeRequested || u.DeviceID != dev || u.DeviceName != "test-dev" || u.ClosedBy != "" ||
		u.FinishedAt != nil || !u.UpdatedAt.Equal(u.StartedAt) || u.Stalled {
		t.Fatalf("new upgrade = %+v", u)
	}
	events, err := st.ListUpgradeEvents(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].State != proto.UpgradeRequested || events[0].Source != UpgradeSourceServer ||
		!events[0].Applied || !events[0].TS.Equal(u.StartedAt) {
		t.Fatalf("events = %+v", events)
	}
	if _, err := st.GetUpgrade(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

// One subtest per row of the applied / not-applied table.
func TestRecordUpgradeEvent(t *testing.T) {
	ctx := context.Background()

	t.Run("in flight takes any valid state", func(t *testing.T) {
		st := openTestDB(t)
		id, _ := st.CreateUpgrade(ctx, newUpgradeDevice(t, st, "dev"), "0.3.0", "0.4.0", "admin")
		before, _ := st.GetUpgrade(ctx, id)
		if !record(t, st, id, UpgradeSourceAgent, proto.UpgradeDownloading, "", "") {
			t.Fatal("downloading not applied")
		}
		u, _ := st.GetUpgrade(ctx, id)
		if u.State != proto.UpgradeDownloading || u.ClosedBy != "" || u.FinishedAt != nil || u.UpdatedAt.Before(before.UpdatedAt) {
			t.Fatalf("after downloading = %+v", u)
		}
		if !record(t, st, id, UpgradeSourceAgent, proto.UpgradeFailed, "selftest_config_rejected", "stderr text") {
			t.Fatal("failed not applied")
		}
		u, _ = st.GetUpgrade(ctx, id)
		if u.State != proto.UpgradeFailed || u.FailureReason != "selftest_config_rejected" || u.FailureDetail != "stderr text" ||
			u.ClosedBy != UpgradeSourceAgent || u.FinishedAt == nil || !u.FinishedAt.Equal(u.UpdatedAt) {
			t.Fatalf("after failed = %+v", u)
		}
		events, _ := st.ListUpgradeEvents(ctx, id)
		if len(events) != 3 || !events[2].Applied || events[2].Detail != "stderr text" || events[2].Source != UpgradeSourceAgent {
			t.Fatalf("events = %+v", events)
		}
	})

	t.Run("annotation adds a line and bumps nothing", func(t *testing.T) {
		st := openTestDB(t)
		id, _ := st.CreateUpgrade(ctx, newUpgradeDevice(t, st, "dev"), "0.3.0", "0.4.0", "admin")
		record(t, st, id, UpgradeSourceAgent, proto.UpgradeRestarting, "", "")
		before, _ := st.GetUpgrade(ctx, id)
		later := before.UpdatedAt.Add(time.Hour)
		applied, err := st.RecordUpgradeEvent(ctx, UpgradeEvent{UpgradeID: id, TS: later, Source: UpgradeSourceServer,
			State: proto.UpgradeRestarting, Reason: UpgradeReasonHandshakeRejected, Detail: "agent 0.4.0 speaks protocol 9"})
		if err != nil || !applied {
			t.Fatalf("annotation: applied=%v err=%v", applied, err)
		}
		u, _ := st.GetUpgrade(ctx, id)
		if u.State != proto.UpgradeRestarting || !u.UpdatedAt.Equal(before.UpdatedAt) || u.FailureReason != "" || u.FailureDetail != "" {
			t.Fatalf("annotation changed the row: %+v", u)
		}
		events, _ := st.ListUpgradeEvents(ctx, id)
		last := events[len(events)-1]
		if len(events) != 3 || last.Reason != UpgradeReasonHandshakeRejected || !last.TS.Equal(later) || !last.Applied {
			t.Fatalf("events = %+v", events)
		}
	})

	t.Run("agent terminal supersedes server and admin outcomes", func(t *testing.T) {
		for _, closer := range []string{UpgradeSourceServer, UpgradeSourceAdmin} {
			st := openTestDB(t)
			id, _ := st.CreateUpgrade(ctx, newUpgradeDevice(t, st, "dev"), "0.3.0", "0.4.0", "admin")
			record(t, st, id, closer, proto.UpgradeFailed, UpgradeReasonTimedOut, "inferred")
			// Same terminal state, different cause: still the agent's word.
			if !record(t, st, id, UpgradeSourceAgent, proto.UpgradeFailed, "download_failed", "status 404") {
				t.Fatalf("%s: agent report not applied", closer)
			}
			u, _ := st.GetUpgrade(ctx, id)
			if u.State != proto.UpgradeFailed || u.FailureReason != "download_failed" || u.FailureDetail != "status 404" || u.ClosedBy != UpgradeSourceAgent {
				t.Fatalf("%s: superseded row = %+v", closer, u)
			}
		}
	})

	t.Run("everything else is stored and not applied", func(t *testing.T) {
		st := openTestDB(t)
		id, _ := st.CreateUpgrade(ctx, newUpgradeDevice(t, st, "dev"), "0.3.0", "0.4.0", "admin")
		record(t, st, id, UpgradeSourceAdmin, proto.UpgradeFailed, UpgradeReasonAbandoned, "")
		// A late in-flight report must not reopen an abandoned row.
		if record(t, st, id, UpgradeSourceAgent, proto.UpgradeVerifying, "", "") {
			t.Fatal("in-flight state applied to a closed row")
		}
		if u, _ := st.GetUpgrade(ctx, id); u.State != proto.UpgradeFailed || u.ClosedBy != UpgradeSourceAdmin || u.FinishedAt == nil {
			t.Fatalf("row reopened: %+v", u)
		}
		if active, _ := st.GetActiveUpgrade(ctx); active != nil {
			t.Fatalf("slot retaken: %+v", active)
		}
		// The server cannot overrule a closed row, nor can anyone overrule
		// the agent's own outcome.
		if record(t, st, id, UpgradeSourceServer, proto.UpgradeVerified, UpgradeReasonHandshakeAtToVersion, "") {
			t.Fatal("server event applied to a closed row")
		}
		record(t, st, id, UpgradeSourceAgent, proto.UpgradeRolledBack, "no_handshake", "")
		for _, src := range []string{UpgradeSourceAgent, UpgradeSourceServer, UpgradeSourceAdmin} {
			if record(t, st, id, src, proto.UpgradeVerified, "", "") {
				t.Fatalf("%s event applied to a row the agent closed", src)
			}
		}
		u, _ := st.GetUpgrade(ctx, id)
		if u.State != proto.UpgradeRolledBack || u.FailureReason != "no_handshake" || u.ClosedBy != UpgradeSourceAgent {
			t.Fatalf("row = %+v", u)
		}
		events, _ := st.ListUpgradeEvents(ctx, id)
		var applied []bool
		for _, e := range events {
			applied = append(applied, e.Applied)
		}
		want := []bool{true, true, false, false, true, false, false, false}
		if !slices.Equal(applied, want) {
			t.Fatalf("applied = %v, want %v", applied, want)
		}
	})

	t.Run("invalid events are refused", func(t *testing.T) {
		st := openTestDB(t)
		id, _ := st.CreateUpgrade(ctx, newUpgradeDevice(t, st, "dev"), "0.3.0", "0.4.0", "admin")
		for _, ev := range []UpgradeEvent{
			{UpgradeID: id, Source: UpgradeSourceAgent, State: "holding_the_slot"},
			{UpgradeID: id, Source: "someone", State: proto.UpgradeFailed},
		} {
			if _, err := st.RecordUpgradeEvent(ctx, ev); !errors.Is(err, ErrInvalidUpgradeEvent) {
				t.Fatalf("%+v: err = %v", ev, err)
			}
		}
		if _, err := st.RecordUpgradeEvent(ctx, UpgradeEvent{UpgradeID: "nope", Source: UpgradeSourceAgent, State: proto.UpgradeFailed}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown upgrade: %v", err)
		}
		if events, _ := st.ListUpgradeEvents(ctx, id); len(events) != 1 {
			t.Fatalf("refused events were stored: %+v", events)
		}
	})
}

func TestUpgradeEventCap(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()
	dev := newUpgradeDevice(t, st, "dev")
	id, _ := st.CreateUpgrade(ctx, dev, "0.3.0", "0.4.0", "admin")
	record(t, st, id, UpgradeSourceAgent, proto.UpgradeRestarting, "", "")
	for i := 2; i < MaxUpgradeEvents; i++ {
		record(t, st, id, UpgradeSourceAgent, proto.UpgradeRestarting, "", "")
	}
	// The 33rd is refused, whether it would annotate or move the row.
	for _, ev := range []UpgradeEvent{
		{UpgradeID: id, Source: UpgradeSourceAgent, State: proto.UpgradeRestarting},
		{UpgradeID: id, Source: UpgradeSourceAgent, State: proto.UpgradeVerified},
		{UpgradeID: id, Source: UpgradeSourceServer, State: proto.UpgradeRestarting, Reason: UpgradeReasonHandshakeRejected},
	} {
		if _, err := st.RecordUpgradeEvent(ctx, ev); !errors.Is(err, ErrTooManyEvents) {
			t.Fatalf("%+v: err = %v, want ErrTooManyEvents", ev, err)
		}
	}
	if events, _ := st.ListUpgradeEvents(ctx, id); len(events) != MaxUpgradeEvents {
		t.Fatalf("events = %d, want %d", len(events), MaxUpgradeEvents)
	}
	if u, _ := st.GetUpgrade(ctx, id); u.State != proto.UpgradeRestarting {
		t.Fatalf("row changed past the cap: %+v", u)
	}
	// A full timeline must not make the upgrade impossible to close.
	if err := st.AbandonUpgrade(ctx, dev, id); err != nil {
		t.Fatalf("abandon at the cap: %v", err)
	}
	if u, _ := st.GetUpgrade(ctx, id); u.State != proto.UpgradeFailed || u.FailureReason != UpgradeReasonAbandoned {
		t.Fatalf("row after abandon = %+v", u)
	}
}

func TestFindUpgradeForResult(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()
	devA, devB := newUpgradeDevice(t, st, "dev-a"), newUpgradeDevice(t, st, "dev-b")
	older, _ := st.CreateUpgrade(ctx, devA, "0.3.0", "0.4.0", "admin")
	record(t, st, older, UpgradeSourceAgent, proto.UpgradeFailed, "verify_failed", "")
	newer, _ := st.CreateUpgrade(ctx, devA, "0.3.0", "0.4.0", "admin")
	record(t, st, newer, UpgradeSourceAgent, proto.UpgradeFailed, "verify_failed", "")
	other, _ := st.CreateUpgrade(ctx, devB, "0.3.0", "0.5.0", "admin")

	// By id: that row, not the newest.
	if u, err := st.FindUpgradeForResult(ctx, devA, older, "0.4.0"); err != nil || u.ID != older {
		t.Fatalf("by id = %+v err=%v", u, err)
	}
	// An id always wins over the version, and never crosses devices.
	if u, err := st.FindUpgradeForResult(ctx, devA, older, "9.9.9"); err != nil || u.ID != older {
		t.Fatalf("by id with other version = %+v err=%v", u, err)
	}
	if _, err := st.FindUpgradeForResult(ctx, devA, other, "0.5.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another device's id: %v", err)
	}
	if _, err := st.FindUpgradeForResult(ctx, devB, older, "0.4.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another device's id: %v", err)
	}
	// Version fallback: the newest row for that version.
	if u, err := st.FindUpgradeForResult(ctx, devA, "", "0.4.0"); err != nil || u.ID != newer {
		t.Fatalf("by version = %+v err=%v, want %s", u, err, newer)
	}
	// No match.
	if _, err := st.FindUpgradeForResult(ctx, devA, "", "0.5.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other device's version: %v", err)
	}
	if _, err := st.FindUpgradeForResult(ctx, devA, "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no version: %v", err)
	}
}

// With no id, an in-flight row is preferred to a newer closed one.
func TestFindUpgradeForResultPrefersInFlight(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()
	dev := newUpgradeDevice(t, st, "dev")
	inFlight, _ := st.CreateUpgrade(ctx, dev, "0.3.0", "0.4.0", "admin")
	closed, _ := st.CreateUpgrade(ctx, dev, "0.3.0", "0.4.0", "admin")
	record(t, st, closed, UpgradeSourceAgent, proto.UpgradeFailed, "busy", "")
	if latest, _ := st.GetLatestUpgrade(ctx, dev); latest.ID != closed {
		t.Fatalf("latest = %s, want the closed row %s", latest.ID, closed)
	}
	if u, err := st.FindUpgradeForResult(ctx, dev, "", "0.4.0"); err != nil || u.ID != inFlight {
		t.Fatalf("found %+v err=%v, want in-flight %s", u, err, inFlight)
	}
}

func TestGetDeviceUpgradesLimit(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()
	dev := newUpgradeDevice(t, st, "dev")
	var ids []string
	for range 5 {
		id, _ := st.CreateUpgrade(ctx, dev, "0.3.0", "0.4.0", "admin")
		record(t, st, id, UpgradeSourceAgent, proto.UpgradeFailed, "busy", "")
		ids = append(ids, id)
	}
	got, err := st.GetDeviceUpgrades(ctx, dev, 2)
	if err != nil || len(got) != 2 || got[0].ID != ids[4] || got[1].ID != ids[3] {
		t.Fatalf("limit 2 = %+v err=%v", got, err)
	}
	if all, _ := st.GetDeviceUpgrades(ctx, dev, 0); len(all) != 5 {
		t.Fatalf("no limit = %d rows", len(all))
	}
}

func TestUpgradeStalled(t *testing.T) {
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		state string
		age   time.Duration
		want  bool
	}{
		{proto.UpgradeSwapped, 2 * time.Minute, false},
		{proto.UpgradeSwapped, 2*time.Minute + time.Second, true},
		{proto.UpgradeRestarting, 5 * time.Minute, false},
		{proto.UpgradeRestarting, 5*time.Minute + time.Second, true},
		// Before the swap a quiet row is timed out, not flagged.
		{proto.UpgradeDownloading, time.Hour, false},
		{proto.UpgradeFailed, time.Hour, false},
	} {
		u := Upgrade{State: c.state, UpdatedAt: at}
		if got := u.IsStalled(at.Add(c.age)); got != c.want {
			t.Errorf("%s after %s: stalled = %v, want %v", c.state, c.age, got, c.want)
		}
	}
}

func TestUpgradeDispatchRoundTrip(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()
	dev := newUpgradeDevice(t, st, "dev")
	if d, _ := st.GetDevice(ctx, "dev"); d.UpgradeDispatch != nil {
		t.Fatalf("new device has a dispatch: %+v", d.UpgradeDispatch)
	}
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	since := at.Add(-12 * time.Minute)
	want := UpgradeDispatch{Code: DispatchInFlight, Message: "another upgrade is in flight: mac-desktop 0.2.4 -> 0.2.5, restarting for 12m",
		BlockingDevice: "mac-desktop", At: at, BlockingState: proto.UpgradeRestarting, BlockingSince: &since}
	if err := st.SetUpgradeDispatch(ctx, dev, want); err != nil {
		t.Fatal(err)
	}
	d, _ := st.GetDevice(ctx, "dev")
	got := d.UpgradeDispatch
	if got == nil || got.Code != want.Code || got.Message != want.Message || got.BlockingDevice != want.BlockingDevice ||
		got.UpgradeID != "" || !got.At.Equal(at) || got.BlockingState != want.BlockingState ||
		got.BlockingSince == nil || !got.BlockingSince.Equal(since) {
		t.Fatalf("round trip = %+v", got)
	}
	// ListDevices reads the same column.
	if all, _ := st.ListDevices(ctx); len(all) != 1 || all[0].UpgradeDispatch == nil || all[0].UpgradeDispatch.Code != DispatchInFlight {
		t.Fatalf("list = %+v", all)
	}
	if err := st.SetUpgradeDispatch(ctx, dev, UpgradeDispatch{}); err != nil {
		t.Fatal(err)
	}
	if d, _ := st.GetDevice(ctx, "dev"); d.UpgradeDispatch != nil {
		t.Fatalf("not cleared: %+v", d.UpgradeDispatch)
	}
	if err := st.SetUpgradeDispatch(ctx, dev+99, want); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown device: %v", err)
	}
}

func TestGetActiveUpgrade(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()
	dev := newUpgradeDevice(t, st, "dev-a")
	id, _ := st.CreateUpgrade(ctx, dev, "0.3.0", "0.4.0", "admin")
	active, err := st.GetActiveUpgrade(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if active == nil || active.ID != id || active.DeviceName != "dev-a" {
		t.Fatalf("active = %+v", active)
	}
	record(t, st, id, UpgradeSourceAgent, proto.UpgradeVerified, "", "")
	active, _ = st.GetActiveUpgrade(ctx)
	if active != nil {
		t.Fatal("expected no active upgrade after verified")
	}
}

func TestHasFailedUpgrade(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()
	dev := newUpgradeDevice(t, st, "dev-b")
	id, _ := st.CreateUpgrade(ctx, dev, "0.3.0", "0.4.0", "admin")
	record(t, st, id, UpgradeSourceAgent, proto.UpgradeFailed, "verify_failed", "")
	if !st.HasFailedUpgrade(ctx, dev, "0.4.0") {
		t.Fatal("expected failed upgrade")
	}
	if st.HasFailedUpgrade(ctx, dev, "0.5.0") {
		t.Fatal("unexpected failed upgrade for different version")
	}
}

func TestAbandonUpgrade(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()
	dev, otherDev := newUpgradeDevice(t, st, "dev-c"), newUpgradeDevice(t, st, "dev-d")
	id, _ := st.CreateUpgrade(ctx, dev, "0.3.0", "0.4.0", "admin")

	if err := st.AbandonUpgrade(ctx, dev, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if err := st.AbandonUpgrade(ctx, otherDev, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong device: %v", err)
	}
	if u, _ := st.GetUpgrade(ctx, id); !u.IsInFlight() {
		t.Fatalf("refused abandon changed the row: %+v", u)
	}

	if err := st.AbandonUpgrade(ctx, dev, id); err != nil {
		t.Fatal(err)
	}
	u, _ := st.GetLatestUpgrade(ctx, dev)
	if u.State != proto.UpgradeFailed || u.FailureReason != UpgradeReasonAbandoned || u.ClosedBy != UpgradeSourceAdmin || u.FinishedAt == nil {
		t.Fatalf("abandoned row = %+v", u)
	}
	events, _ := st.ListUpgradeEvents(ctx, id)
	if last := events[len(events)-1]; len(events) != 2 || last.Source != UpgradeSourceAdmin || last.Reason != UpgradeReasonAbandoned || !last.Applied {
		t.Fatalf("events = %+v", events)
	}
	if err := st.AbandonUpgrade(ctx, dev, id); !errors.Is(err, ErrUpgradeNotInFlight) {
		t.Fatalf("second abandon: %v", err)
	}
	if events, _ := st.ListUpgradeEvents(ctx, id); len(events) != 2 {
		t.Fatalf("refused abandon added an event: %+v", events)
	}
}
