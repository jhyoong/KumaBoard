package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
)

// Window is one expected-offline period. Days is "*" or a comma list of
// three-letter day names ("mon,tue"). From and To are "HH:MM" server local time.
type Window struct {
	Days string `json:"days"`
	From string `json:"from"`
	To   string `json:"to"`
}

// Schedule is a device's expected downtime configuration.
type Schedule struct {
	ExpectedOffline []Window `json:"expected_offline"`
	GracePeriodS    int      `json:"grace_period_s"`
}

// Device is one row of the devices table.
type Device struct {
	ID                  int64
	Name                string
	MAC                 string
	OS                  string
	Arch                string
	TokenHash           []byte
	Capabilities        []string
	Schedule            Schedule
	NormallyOff         bool
	TerminalEnabled     bool
	LastSeen            *time.Time
	LastDisconnectAt    *time.Time
	LastRejectReason    string
	AgentVersion        string
	DesiredAgentVersion string
	ProtocolVersion     int
	CreatedAt           time.Time
}

// GenerateToken returns a new 32-byte CSPRNG token and its SHA-256 hash.
func GenerateToken() (plain string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	plain = base64.StdEncoding.EncodeToString(b)
	return plain, HashToken(plain), nil
}

// HashToken hashes a plaintext token for storage or comparison.
func HashToken(plain string) []byte {
	h := sha256.Sum256([]byte(plain))
	return h[:]
}

const deviceCols = `id, name, mac, os, arch, token_hash, capabilities_json, schedule_json,
	normally_off, terminal_enabled, last_seen, last_disconnect_at, last_reject_reason,
	agent_version, desired_agent_version, protocol_version, created_at`

type scanner interface{ Scan(dest ...any) error }

func scanDevice(row scanner) (*Device, error) {
	var d Device
	var caps, sched, created string
	var lastSeen, lastDisc sql.NullString
	var normallyOff, terminalEnabled int
	err := row.Scan(&d.ID, &d.Name, &d.MAC, &d.OS, &d.Arch, &d.TokenHash, &caps, &sched,
		&normallyOff, &terminalEnabled, &lastSeen, &lastDisc, &d.LastRejectReason,
		&d.AgentVersion, &d.DesiredAgentVersion, &d.ProtocolVersion, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(caps), &d.Capabilities)
	_ = json.Unmarshal([]byte(sched), &d.Schedule)
	if d.Capabilities == nil {
		d.Capabilities = []string{}
	}
	d.NormallyOff = normallyOff == 1
	d.TerminalEnabled = terminalEnabled == 1
	d.LastSeen = parseTime(lastSeen)
	d.LastDisconnectAt = parseTime(lastDisc)
	d.CreatedAt = mustTime(created)
	return &d, nil
}

// CreateDevice registers a device and returns the plaintext token exactly once.
func (s *Store) CreateDevice(ctx context.Context, name, mac string, normallyOff bool, sched Schedule) (*Device, string, error) {
	plain, hash, err := GenerateToken()
	if err != nil {
		return nil, "", err
	}
	schedJSON, _ := json.Marshal(sched)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO devices (name, mac, token_hash, schedule_json, normally_off, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		name, mac, hash, string(schedJSON), b2i(normallyOff), nowString())
	if isUniqueViolation(err) {
		return nil, "", ErrExists
	}
	if err != nil {
		return nil, "", err
	}
	d, err := s.GetDevice(ctx, name)
	if err != nil {
		return nil, "", err
	}
	return d, plain, nil
}

// GetDevice looks a device up by name.
func (s *Store) GetDevice(ctx context.Context, name string) (*Device, error) {
	return scanDevice(s.db.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE name = ?`, name))
}

// GetDeviceByID looks a device up by id.
func (s *Store) GetDeviceByID(ctx context.Context, id int64) (*Device, error) {
	return scanDevice(s.db.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE id = ?`, id))
}

// ListDevices returns all devices ordered by name.
func (s *Store) ListDevices(ctx context.Context) ([]*Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deviceCols+` FROM devices ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateDeviceSettings changes the operator-editable fields.
func (s *Store) UpdateDeviceSettings(ctx context.Context, name, mac string, normallyOff bool, sched Schedule, terminalEnabled bool) error {
	schedJSON, _ := json.Marshal(sched)
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET mac = ?, normally_off = ?, schedule_json = ?, terminal_enabled = ? WHERE name = ?`,
		mac, b2i(normallyOff), string(schedJSON), b2i(terminalEnabled), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeToken clears the token hash so the device can no longer authenticate.
func (s *Store) RevokeToken(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET token_hash = NULL WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordHandshake stores what the agent declared and refreshes its command list.
func (s *Store) RecordHandshake(ctx context.Context, deviceID int64, osName, arch, agentVersion string, protocolVersion int, caps []string, cmds []proto.CommandDef) error {
	if caps == nil {
		caps = []string{}
	}
	capsJSON, _ := json.Marshal(caps)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := nowString()
	if _, err := tx.ExecContext(ctx,
		`UPDATE devices SET os = ?, arch = ?, agent_version = ?, protocol_version = ?, capabilities_json = ?,
		 last_seen = ?, last_reject_reason = '' WHERE id = ?`,
		osName, arch, agentVersion, protocolVersion, string(capsJSON), now, deviceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM commands WHERE device_id = ?`, deviceID); err != nil {
		return err
	}
	for _, c := range cmds {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO commands (device_id, name, description, timeout_s, expect_disconnect, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			deviceID, c.Name, c.Description, c.TimeoutS, b2i(c.ExpectDisconnect), now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListCommands returns the commands the device declared at its last handshake.
func (s *Store) ListCommands(ctx context.Context, deviceID int64) ([]proto.CommandDef, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, description, timeout_s, expect_disconnect FROM commands WHERE device_id = ? ORDER BY name`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []proto.CommandDef{}
	for rows.Next() {
		var c proto.CommandDef
		var ed int
		if err := rows.Scan(&c.Name, &c.Description, &c.TimeoutS, &ed); err != nil {
			return nil, err
		}
		c.ExpectDisconnect = ed == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

// RecordReject stores why the last handshake was refused.
func (s *Store) RecordReject(ctx context.Context, name, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE devices SET last_reject_reason = ? WHERE name = ?`, reason, name)
	return err
}

// RecordDisconnect stamps last_disconnect_at.
func (s *Store) RecordDisconnect(ctx context.Context, deviceID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE devices SET last_disconnect_at = ? WHERE id = ?`, nowString(), deviceID)
	return err
}

// TouchLastSeen stamps last_seen.
func (s *Store) TouchLastSeen(ctx context.Context, deviceID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE devices SET last_seen = ? WHERE id = ?`, nowString(), deviceID)
	return err
}

// InsertTerminalSession records a new terminal session for a device.
func (s *Store) InsertTerminalSession(ctx context.Context, id string, deviceID int64, user string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO terminal_sessions (id, device_id, user, started_at) VALUES (?, ?, ?, ?)`,
		id, deviceID, user, nowString())
	return err
}

// CloseTerminalSession marks a session as ended and records byte counts.
func (s *Store) CloseTerminalSession(ctx context.Context, id string, bytesIn, bytesOut int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE terminal_sessions SET ended_at = ?, bytes_in = ?, bytes_out = ? WHERE id = ?`,
		nowString(), bytesIn, bytesOut, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountActiveTerminalSessions returns the number of open sessions for a device.
func (s *Store) CountActiveTerminalSessions(ctx context.Context, deviceID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM terminal_sessions WHERE device_id = ? AND ended_at IS NULL`, deviceID).Scan(&n)
	return n, err
}

// EndOpenTerminalSessions closes every terminal_sessions row still open. Live
// sessions never outlive the server process, so at startup any open row is an
// orphan from a crash. It returns the number of rows closed.
func (s *Store) EndOpenTerminalSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE terminal_sessions SET ended_at = ? WHERE ended_at IS NULL`, nowString())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
