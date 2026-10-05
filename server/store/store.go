// Package store is the SQLite persistence layer.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

var (
	ErrNotFound = errors.New("store: not found")
	ErrExists   = errors.New("store: already exists")
)

// Store wraps the database handle.
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path and applies migrations.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// One writer at a time keeps SQLite simple. Seven devices do not need more.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var version int
		if _, err := fmt.Sscanf(name, "%d_", &version); err != nil {
			return fmt.Errorf("store: bad migration name %q", name)
		}
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, nowString()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// nowString is the canonical stored time format.
func nowString() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func timeString(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s.String)
	if err != nil {
		return nil
	}
	return &t
}

func mustTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// Audit appends one audit_log row. Never fails the caller's operation: errors
// are returned so the caller can log them, but callers should not abort on them.
func (s *Store) Audit(ctx context.Context, actor, action, target, result, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (ts, actor, action, target, result, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		nowString(), actor, action, target, result, detail)
	return err
}

// AuditEntry is one audit_log row.
type AuditEntry struct {
	ID     int64     `json:"id"`
	TS     time.Time `json:"ts"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target"`
	Result string    `json:"result"`
	Detail string    `json:"detail"`
}

// ListAudit returns up to limit rows with id < beforeID (0 means newest), newest first.
func (s *Store) ListAudit(ctx context.Context, limit int, beforeID int64) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if beforeID <= 0 {
		beforeID = 1<<62 - 1
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ts, actor, action, target, result, detail FROM audit_log WHERE id < ? ORDER BY id DESC LIMIT ?`,
		beforeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts string
		if err := rows.Scan(&e.ID, &ts, &e.Actor, &e.Action, &e.Target, &e.Result, &e.Detail); err != nil {
			return nil, err
		}
		e.TS = mustTime(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}
