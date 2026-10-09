package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenCreatesSchema(t *testing.T) {
	s := openTest(t)
	want := []string{"devices", "commands", "command_runs", "terminal_sessions",
		"audit_log", "users", "sessions", "wake_jobs", "agent_upgrades", "releases", "metrics_samples", "metrics_rollup", "command_problems", "agent_upgrade_events"}
	for _, table := range want {
		var n int
		err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
		if err != nil || n != 1 {
			t.Errorf("table %s missing (err=%v)", table, err)
		}
	}
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q err=%v, want wal", mode, err)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s1.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second open failed: %v", err)
	}
	defer s2.Close()
	var n int
	if err := s2.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil || n != 7 {
		t.Fatalf("migrations applied = %d err=%v, want 7", n, err)
	}
}

// TestMigrateTempFrom0004 opens a database left at schema 0004 and checks
// 0005 is applied on the next Open, with pre-existing rows reading no
// temperature rather than zero.
func TestMigrateTempFrom0004(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for v, name := range []string{"0001_init.sql", "0002_metrics_samples.sql", "0003_metrics_rollup.sql", "0004_metrics_gpu_mem.sql"} {
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		db.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, v+1, nowString())
	}
	// Raw SQL: Store methods expect the current schema, which this is not yet.
	res, err := db.Exec(`INSERT INTO devices (name, created_at) VALUES ('deb', ?)`, nowString())
	if err != nil {
		t.Fatal(err)
	}
	var dev struct{ ID int64 }
	if dev.ID, err = res.LastInsertId(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO metrics_samples (device_id, bucket, cpu_percent, mem_used_bytes, mem_total_bytes, disk_used_percent)
		VALUES (?, 1800000000, 12, 1, 2, 3)`, dev.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO metrics_rollup (device_id, bucket, cpu_percent, mem_used_bytes, mem_total_bytes, disk_used_percent)
		VALUES (?, 1799999100, 11, 1, 2, 3)`, dev.ID); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 5`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("0005 applied = %d err=%v", n, err)
	}
	hot, err := s.MetricsHistory(ctx, "deb", time.Unix(1_799_990_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(hot) != 1 || hot[0].CPUPercent != 12 || hot[0].TempC != nil {
		t.Fatalf("old sample = %+v", hot)
	}
	roll, err := s.MetricsRollupHistory(ctx, "deb", time.Unix(1_799_990_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(roll) != 2 || roll[0].CPUPercent != 11 || roll[0].TempC != nil || roll[1].TempC != nil {
		t.Fatalf("old rollup = %+v", roll)
	}
	// New samples on the migrated database carry temperature.
	if err := s.RecordMetrics(ctx, dev.ID, time.Unix(1_800_000_030, 0), proto.Metrics{TempC: f64(48)}); err != nil {
		t.Fatal(err)
	}
	if hot, _ := s.MetricsHistory(ctx, "deb", time.Unix(1_800_000_030, 0)); len(hot) != 1 || hot[0].TempC == nil || *hot[0].TempC != 48 {
		t.Fatalf("new sample = %+v", hot)
	}
}

// TestMigrateCommandsFrom0005 opens a database left at schema 0005 and checks
// 0006 is applied on the next Open, with pre-existing commands reading
// confirm=false and no problems or config error.
func TestMigrateCommandsFrom0005(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for v, name := range []string{"0001_init.sql", "0002_metrics_samples.sql", "0003_metrics_rollup.sql", "0004_metrics_gpu_mem.sql", "0005_metrics_temp.sql"} {
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		db.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, v+1, nowString())
	}
	res, err := db.Exec(`INSERT INTO devices (name, created_at) VALUES ('deb', ?)`, nowString())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if _, err := db.Exec(`INSERT INTO commands (device_id, name, timeout_s, expect_disconnect, updated_at) VALUES (?, 'sleep', 15, 1, ?)`, id, nowString()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO command_runs (id, device_id, command, requested_by, requested_at, status) VALUES ('r1', ?, 'sleep', 'admin', ?, 'ok')`, id, nowString()); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 6`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("0006 applied = %d err=%v", n, err)
	}
	cmds, err := s.ListCommands(ctx, id)
	if err != nil || len(cmds) != 1 || cmds[0].Name != "sleep" || cmds[0].Confirm || !cmds[0].ExpectDisconnect {
		t.Fatalf("old command = %+v err=%v", cmds, err)
	}
	if p, err := s.ListCommandProblems(ctx, id); err != nil || len(p) != 0 {
		t.Fatalf("problems = %+v err=%v", p, err)
	}
	d, err := s.GetDevice(ctx, "deb")
	if err != nil || d.CommandsConfigError != "" {
		t.Fatalf("device = %+v err=%v", d, err)
	}
	// command_runs is untouched and takes the new free-text statuses.
	if r, err := s.GetRun(ctx, "r1"); err != nil || r.Status != proto.RunOK {
		t.Fatalf("old run = %+v err=%v", r, err)
	}
	s.InsertRun(ctx, &Run{ID: "r2", DeviceID: id, Command: "sleep", RequestedBy: "admin", RequestedAt: time.Now(), Status: proto.RunRunning})
	s.FinishRun(ctx, "r2", proto.RunCancelled, nil, "", "", false)
	if r, _ := s.GetRun(ctx, "r2"); r == nil || r.Status != proto.RunCancelled {
		t.Fatalf("cancelled run = %+v", r)
	}
}

// TestMigrateUpgradesFrom0006 opens a database left at schema 0006 with
// upgrade rows in it and checks 0007 backfills updated_at and closed_by, and
// that the old rows read back with no events, detail or dispatch status.
func TestMigrateUpgradesFrom0006(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for v, name := range []string{"0001_init.sql", "0002_metrics_samples.sql", "0003_metrics_rollup.sql",
		"0004_metrics_gpu_mem.sql", "0005_metrics_temp.sql", "0006_commands_confirm.sql"} {
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		db.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, v+1, nowString())
	}
	res, err := db.Exec(`INSERT INTO devices (name, created_at) VALUES ('deb', ?)`, nowString())
	if err != nil {
		t.Fatal(err)
	}
	dev, _ := res.LastInsertId()
	started := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	old := []struct {
		id, state, reason string
		finished          bool
		wantClosedBy      string
	}{
		{"u1", "failed", "selftest_failed", true, UpgradeSourceAgent},
		{"u2", "failed", "abandoned", true, UpgradeSourceAdmin},
		{"u3", "rolled_back", "handshake_at_from_version", true, UpgradeSourceServer},
		{"u4", "failed", "send_failed", true, UpgradeSourceServer},
		{"u5", "verified", "", true, UpgradeSourceAgent},
		{"u6", "restarting", "", false, ""},
	}
	for i, o := range old {
		at := started.Add(time.Duration(i) * time.Hour)
		var finished any
		if o.finished {
			finished = timeString(at.Add(time.Minute))
		}
		if _, err := db.Exec(`INSERT INTO agent_upgrades (id, device_id, from_version, to_version, requested_by, started_at, finished_at, state, failure_reason)
			VALUES (?, ?, '0.2.4', '0.2.5', 'admin', ?, ?, ?, ?)`, o.id, dev, timeString(at), finished, o.state, o.reason); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = 7`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("0007 applied = %d err=%v", n, err)
	}
	for i, o := range old {
		at := started.Add(time.Duration(i) * time.Hour)
		u, err := s.GetUpgrade(ctx, o.id)
		if err != nil {
			t.Fatalf("%s: %v", o.id, err)
		}
		wantUpdated := at
		if o.finished {
			wantUpdated = at.Add(time.Minute)
		}
		if !u.UpdatedAt.Equal(wantUpdated) || u.ClosedBy != o.wantClosedBy || u.FailureDetail != "" ||
			u.State != o.state || u.FailureReason != o.reason || u.DeviceName != "deb" {
			t.Errorf("%s = %+v, want updated_at %v closed_by %q", o.id, u, wantUpdated, o.wantClosedBy)
		}
		if ev, err := s.ListUpgradeEvents(ctx, o.id); err != nil || len(ev) != 0 {
			t.Errorf("%s events = %+v err=%v, want none", o.id, ev, err)
		}
	}
	d, err := s.GetDevice(ctx, "deb")
	if err != nil || d.UpgradeDispatch != nil {
		t.Fatalf("device = %+v err=%v", d, err)
	}
	// The row left in flight still holds the slot, is flagged as stalled,
	// and takes events like any new one.
	active, err := s.GetActiveUpgrade(ctx)
	if err != nil || active == nil || active.ID != "u6" || !active.Stalled {
		t.Fatalf("active = %+v err=%v", active, err)
	}
	if applied, err := s.RecordUpgradeEvent(ctx, UpgradeEvent{UpgradeID: "u6", Source: UpgradeSourceAgent, State: proto.UpgradeVerified}); err != nil || !applied {
		t.Fatalf("event on old row: applied=%v err=%v", applied, err)
	}
	if u, _ := s.GetUpgrade(ctx, "u6"); u.State != proto.UpgradeVerified || u.ClosedBy != UpgradeSourceAgent || u.FinishedAt == nil {
		t.Fatalf("old row after event = %+v", u)
	}
}
