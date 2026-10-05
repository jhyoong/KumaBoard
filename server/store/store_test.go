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
		"audit_log", "users", "sessions", "wake_jobs", "agent_upgrades", "releases", "metrics_samples", "metrics_rollup"}
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
	if err := s2.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil || n != 5 {
		t.Fatalf("migrations applied = %d err=%v, want 5", n, err)
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
	old := &Store{db: db}
	dev, _, err := old.CreateDevice(ctx, "deb", "", false, Schedule{})
	if err != nil {
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
