package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
)

// Run is one command_runs row.
type Run struct {
	ID          string     `json:"id"`
	DeviceID    int64      `json:"device_id"`
	DeviceName  string     `json:"device"`
	Command     string     `json:"command"`
	RequestedBy string     `json:"requested_by"`
	RequestedAt time.Time  `json:"requested_at"`
	StartedAt   *time.Time `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at"`
	ExitCode    *int       `json:"exit_code"`
	Status      string     `json:"status"`
	StdoutTail  string     `json:"stdout_tail"`
	StderrTail  string     `json:"stderr_tail"`
	Truncated   bool       `json:"truncated"`
}

const runCols = `r.id, r.device_id, d.name, r.command, r.requested_by, r.requested_at, r.started_at,
	r.finished_at, r.exit_code, r.status, r.stdout_tail, r.stderr_tail, r.truncated`

func scanRun(row scanner) (*Run, error) {
	var r Run
	var reqAt string
	var started, finished sql.NullString
	var exit sql.NullInt64
	var trunc int
	err := row.Scan(&r.ID, &r.DeviceID, &r.DeviceName, &r.Command, &r.RequestedBy, &reqAt, &started,
		&finished, &exit, &r.Status, &r.StdoutTail, &r.StderrTail, &trunc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.RequestedAt = mustTime(reqAt)
	r.StartedAt = parseTime(started)
	r.FinishedAt = parseTime(finished)
	if exit.Valid {
		v := int(exit.Int64)
		r.ExitCode = &v
	}
	r.Truncated = trunc == 1
	return &r, nil
}

// InsertRun records a new run. StartedAt is set to RequestedAt.
func (s *Store) InsertRun(ctx context.Context, r *Run) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO command_runs (id, device_id, command, requested_by, requested_at, started_at, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.DeviceID, r.Command, r.RequestedBy, timeString(r.RequestedAt), timeString(r.RequestedAt), r.Status)
	return err
}

// SetRunStatus changes status only (used for dispatched).
func (s *Store) SetRunStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE command_runs SET status = ? WHERE id = ?`, status, id)
	return err
}

// FinishRun records a terminal status and output.
func (s *Store) FinishRun(ctx context.Context, id, status string, exitCode *int, stdout, stderr string, truncated bool) error {
	var exit any
	if exitCode != nil {
		exit = *exitCode
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE command_runs SET status = ?, exit_code = ?, stdout_tail = ?, stderr_tail = ?, truncated = ?, finished_at = ?
		 WHERE id = ?`,
		status, exit, stdout, stderr, b2i(truncated), nowString(), id)
	return err
}

// MarkDeviceRunsLost sets every running row for a device to lost.
// Dispatched rows are handled by the router, which decides between
// disconnected_as_expected and lost.
func (s *Store) MarkDeviceRunsLost(ctx context.Context, deviceID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE command_runs SET status = ?, finished_at = ? WHERE device_id = ? AND status = ?`,
		proto.RunLost, nowString(), deviceID, proto.RunRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MarkAllInFlightLost is run once at server startup.
func (s *Store) MarkAllInFlightLost(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE command_runs SET status = ?, finished_at = ? WHERE status IN (?, ?)`,
		proto.RunLost, nowString(), proto.RunRunning, proto.RunDispatched)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetRun fetches one run.
func (s *Store) GetRun(ctx context.Context, id string) (*Run, error) {
	return scanRun(s.db.QueryRowContext(ctx,
		`SELECT `+runCols+` FROM command_runs r JOIN devices d ON d.id = r.device_id WHERE r.id = ?`, id))
}

// ListRuns returns the newest runs for a device.
func (s *Store) ListRuns(ctx context.Context, deviceID int64, limit int) ([]*Run, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runCols+` FROM command_runs r JOIN devices d ON d.id = r.device_id
		 WHERE r.device_id = ? ORDER BY r.requested_at DESC LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
