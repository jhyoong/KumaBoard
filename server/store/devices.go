package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
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
	// CommandsConfigError is why the agent's last config reload was rejected,
	// as reported by the agent; empty when its commands are current.
	CommandsConfigError string
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
	agent_version, desired_agent_version, protocol_version, commands_config_error, created_at`

type scanner interface{ Scan(dest ...any) error }

func scanDevice(row scanner) (*Device, error) {
	var d Device
	var caps, sched, created string
	var lastSeen, lastDisc sql.NullString
	var normallyOff, terminalEnabled int
	err := row.Scan(&d.ID, &d.Name, &d.MAC, &d.OS, &d.Arch, &d.TokenHash, &caps, &sched,
		&normallyOff, &terminalEnabled, &lastSeen, &lastDisc, &d.LastRejectReason,
		&d.AgentVersion, &d.DesiredAgentVersion, &d.ProtocolVersion, &d.CommandsConfigError, &created)
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

// RecordHandshake stores what the agent declared and refreshes its command
// list. hello carries no config error, so any stored one is cleared; an agent
// that still has one reports it again right after connecting.
func (s *Store) RecordHandshake(ctx context.Context, deviceID int64, osName, arch, agentVersion string, protocolVersion int, caps []string, cmds []proto.CommandDef, problems []proto.CommandProblem) error {
	if caps == nil {
		caps = []string{}
	}
	capsJSON, _ := json.Marshal(caps)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE devices SET os = ?, arch = ?, agent_version = ?, protocol_version = ?, capabilities_json = ?,
		 last_seen = ?, last_reject_reason = '' WHERE id = ?`,
		osName, arch, agentVersion, protocolVersion, string(capsJSON), nowString(), deviceID); err != nil {
		return err
	}
	if _, err := replaceCommands(ctx, tx, deviceID, cmds, problems, ""); err != nil {
		return err
	}
	return tx.Commit()
}

// CommandsChange is what a ReplaceCommands call altered.
type CommandsChange struct {
	Added   []string // command names newly declared
	Removed []string // command names no longer declared
	// Changed is true if anything stored differs at all, including a
	// description, the problem list or the config error.
	Changed bool
}

// ReplaceCommands replaces a device's declared commands, withheld-command
// problems and config error in one transaction. It is the command half of
// RecordHandshake, used when an agent reloads its config while connected.
func (s *Store) ReplaceCommands(ctx context.Context, deviceID int64, cmds []proto.CommandDef, problems []proto.CommandProblem, configError string) (CommandsChange, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CommandsChange{}, err
	}
	defer tx.Rollback()
	ch, err := replaceCommands(ctx, tx, deviceID, cmds, problems, configError)
	if err != nil {
		return CommandsChange{}, err
	}
	return ch, tx.Commit()
}

func replaceCommands(ctx context.Context, tx *sql.Tx, deviceID int64, cmds []proto.CommandDef, problems []proto.CommandProblem, configError string) (CommandsChange, error) {
	var ch CommandsChange
	oldCmds, err := listCommands(ctx, tx, deviceID)
	if err != nil {
		return ch, err
	}
	oldProblems, err := listCommandProblems(ctx, tx, deviceID)
	if err != nil {
		return ch, err
	}
	var oldErr string
	if err := tx.QueryRowContext(ctx, `SELECT commands_config_error FROM devices WHERE id = ?`, deviceID).Scan(&oldErr); err != nil {
		return ch, err
	}

	now := nowString()
	if _, err := tx.ExecContext(ctx, `DELETE FROM commands WHERE device_id = ?`, deviceID); err != nil {
		return ch, err
	}
	for _, c := range cmds {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO commands (device_id, name, description, timeout_s, expect_disconnect, confirm, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			deviceID, c.Name, c.Description, c.TimeoutS, b2i(c.ExpectDisconnect), b2i(c.Confirm), now); err != nil {
			return ch, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM command_problems WHERE device_id = ?`, deviceID); err != nil {
		return ch, err
	}
	for _, p := range problems {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO command_problems (device_id, name, reason) VALUES (?, ?, ?)`, deviceID, p.Name, p.Reason); err != nil {
			return ch, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE devices SET commands_config_error = ? WHERE id = ?`, configError, deviceID); err != nil {
		return ch, err
	}

	newCmds, err := listCommands(ctx, tx, deviceID)
	if err != nil {
		return ch, err
	}
	newProblems, err := listCommandProblems(ctx, tx, deviceID)
	if err != nil {
		return ch, err
	}
	had, has := map[string]bool{}, map[string]bool{}
	for _, c := range oldCmds {
		had[c.Name] = true
	}
	for _, c := range newCmds {
		has[c.Name] = true
		if !had[c.Name] {
			ch.Added = append(ch.Added, c.Name)
		}
	}
	for _, c := range oldCmds {
		if !has[c.Name] {
			ch.Removed = append(ch.Removed, c.Name)
		}
	}
	ch.Changed = oldErr != configError || !slices.Equal(oldCmds, newCmds) || !slices.Equal(oldProblems, newProblems)
	return ch, nil
}

// querier is the read half shared by *sql.DB and *sql.Tx.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// ListCommands returns the commands the device last declared, at its
// handshake or in a later commands_update.
func (s *Store) ListCommands(ctx context.Context, deviceID int64) ([]proto.CommandDef, error) {
	return listCommands(ctx, s.db, deviceID)
}

func listCommands(ctx context.Context, q querier, deviceID int64) ([]proto.CommandDef, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT name, description, timeout_s, expect_disconnect, confirm FROM commands WHERE device_id = ? ORDER BY name`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []proto.CommandDef{}
	for rows.Next() {
		var c proto.CommandDef
		var ed, confirm int
		if err := rows.Scan(&c.Name, &c.Description, &c.TimeoutS, &ed, &confirm); err != nil {
			return nil, err
		}
		c.ExpectDisconnect = ed == 1
		c.Confirm = confirm == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListCommandProblems returns the commands the device has in its config but
// did not declare, with the agent's reason for each.
func (s *Store) ListCommandProblems(ctx context.Context, deviceID int64) ([]proto.CommandProblem, error) {
	return listCommandProblems(ctx, s.db, deviceID)
}

func listCommandProblems(ctx context.Context, q querier, deviceID int64) ([]proto.CommandProblem, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT name, reason FROM command_problems WHERE device_id = ? ORDER BY name`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []proto.CommandProblem{}
	for rows.Next() {
		var p proto.CommandProblem
		if err := rows.Scan(&p.Name, &p.Reason); err != nil {
			return nil, err
		}
		out = append(out, p)
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
