package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/oklog/ulid/v2"
)

type Release struct {
	Version   string    `json:"version"`
	OS        string    `json:"os"`
	Arch      string    `json:"arch"`
	SHA256    string    `json:"sha256"`
	Signature string    `json:"signature"`
	SizeBytes int64     `json:"size_bytes"`
	AddedAt   time.Time `json:"added_at"`
}

func (s *Store) IngestRelease(ctx context.Context, version, os, arch, sha256, signature string, sizeBytes int64) error {
	now := nowString()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO releases (version, os, arch, sha256, signature, size_bytes, added_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(version, os, arch) DO UPDATE SET sha256=?, signature=?, size_bytes=?, added_at=?`,
		version, os, arch, sha256, signature, sizeBytes, now,
		sha256, signature, sizeBytes, now)
	return err
}

func (s *Store) GetRelease(ctx context.Context, version, os, arch string) (*Release, error) {
	var r Release
	var addedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT version, os, arch, sha256, signature, size_bytes, added_at FROM releases WHERE version=? AND os=? AND arch=?`,
		version, os, arch).Scan(&r.Version, &r.OS, &r.Arch, &r.SHA256, &r.Signature, &r.SizeBytes, &addedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.AddedAt = mustTime(addedAt)
	return &r, nil
}

func (s *Store) ListReleases(ctx context.Context, os, arch string) ([]Release, error) {
	var query string
	var args []any
	if os != "" && arch != "" {
		query = `SELECT version, os, arch, sha256, signature, size_bytes, added_at FROM releases WHERE os=? AND arch=? ORDER BY added_at DESC`
		args = []any{os, arch}
	} else {
		query = `SELECT version, os, arch, sha256, signature, size_bytes, added_at FROM releases ORDER BY added_at DESC`
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		var r Release
		var addedAt string
		if err := rows.Scan(&r.Version, &r.OS, &r.Arch, &r.SHA256, &r.Signature, &r.SizeBytes, &addedAt); err != nil {
			return nil, err
		}
		r.AddedAt = mustTime(addedAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Who wrote an upgrade event or closed an upgrade.
const (
	UpgradeSourceAgent  = "agent"
	UpgradeSourceServer = "server"
	UpgradeSourceAdmin  = "admin"
)

// Upgrade reasons the server writes itself. Agent-reported reasons are in
// proto.
const (
	UpgradeReasonAbandoned              = "abandoned"
	UpgradeReasonSendFailed             = "send_failed"
	UpgradeReasonTimedOut               = "timed_out"
	UpgradeReasonHandshakeAtFromVersion = "handshake_at_from_version"
	UpgradeReasonHandshakeAtToVersion   = "handshake_at_to_version"
	UpgradeReasonHandshakeRejected      = "handshake_rejected"
)

// MaxUpgradeEvents caps the timeline of one upgrade. An honest flow produces
// at most nine events.
const MaxUpgradeEvents = 32

var (
	ErrUpgradeNotInFlight  = errors.New("store: upgrade is not in flight")
	ErrTooManyEvents       = errors.New("store: too many events for this upgrade")
	ErrInvalidUpgradeEvent = errors.New("store: invalid upgrade event")
)

// Upgrade is one agent_upgrades row: the summary of an upgrade attempt. Its
// timeline is in agent_upgrade_events.
type Upgrade struct {
	ID       string `json:"id"`
	DeviceID int64  `json:"device_id"`
	// DeviceName is the name of the device the upgrade belongs to.
	DeviceName  string     `json:"-"`
	FromVersion string     `json:"from_version"`
	ToVersion   string     `json:"to_version"`
	RequestedBy string     `json:"requested_by"`
	StartedAt   time.Time  `json:"started_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	FinishedAt  *time.Time `json:"finished_at"`
	State       string     `json:"state"`
	// FailureReason and FailureDetail come from the event that set State.
	// When ClosedBy is "agent" the detail is the agent's own text: display
	// only.
	FailureReason string `json:"failure_reason"`
	FailureDetail string `json:"failure_detail"`
	// ClosedBy is who set the terminal state: agent, server or admin. Empty
	// while the upgrade is in flight.
	ClosedBy string `json:"closed_by"`
	// Stalled is IsStalled at the time the row was read.
	Stalled bool `json:"stalled"`
}

func isTerminalUpgradeState(state string) bool {
	switch state {
	case proto.UpgradeVerified, proto.UpgradeRolledBack, proto.UpgradeFailed:
		return true
	}
	return false
}

func (u *Upgrade) IsInFlight() bool { return !isTerminalUpgradeState(u.State) }

// UpgradeStaleAfter is how long an upgrade may sit in an in-flight state
// without a report before it counts as stale. ok is false for terminal and
// unknown states.
func UpgradeStaleAfter(state string) (limit time.Duration, ok bool) {
	switch state {
	case proto.UpgradeRequested, proto.UpgradeSelftest, proto.UpgradeSwapped:
		return 2 * time.Minute, true
	case proto.UpgradeVerifying, proto.UpgradeRestarting:
		return 5 * time.Minute, true
	case proto.UpgradeDownloading:
		return 15 * time.Minute, true
	}
	return 0, false
}

// UpgradePastSwap reports whether state is an in-flight state in which the
// device's binary has already been replaced. Such an upgrade is never timed
// out by the server; it is only flagged as stalled.
func UpgradePastSwap(state string) bool {
	return state == proto.UpgradeSwapped || state == proto.UpgradeRestarting
}

// IsStalled reports whether the upgrade is past the swap and has had no
// report for longer than its state allows. Earlier states are not flagged:
// the server times those out instead.
func (u *Upgrade) IsStalled(now time.Time) bool {
	if !UpgradePastSwap(u.State) {
		return false
	}
	limit, _ := UpgradeStaleAfter(u.State)
	return now.Sub(u.UpdatedAt) > limit
}

// UpgradeEvent is one agent_upgrade_events row: a line of an upgrade's
// timeline.
type UpgradeEvent struct {
	UpgradeID string `json:"-"`
	// TS is when the event happened. RecordUpgradeEvent uses the current
	// time when it is zero.
	TS     time.Time `json:"ts"`
	Source string    `json:"source"`
	State  string    `json:"state"`
	Reason string    `json:"reason"`
	Detail string    `json:"detail"`
	// Applied is false for an event that was recorded but did not change
	// the upgrade. RecordUpgradeEvent sets it; callers leave it alone.
	Applied bool `json:"applied"`
}

const upgradeSelect = `SELECT u.id, u.device_id, d.name, u.from_version, u.to_version, u.requested_by,
	u.started_at, u.updated_at, u.finished_at, u.state, u.failure_reason, u.failure_detail, u.closed_by
	FROM agent_upgrades u JOIN devices d ON d.id = u.device_id `

// rowQuerier is the single-row read shared by *sql.DB and *sql.Tx.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func scanUpgrade(row scanner) (*Upgrade, error) {
	var u Upgrade
	var startedAt, updatedAt string
	var finishedAt sql.NullString
	err := row.Scan(&u.ID, &u.DeviceID, &u.DeviceName, &u.FromVersion, &u.ToVersion, &u.RequestedBy,
		&startedAt, &updatedAt, &finishedAt, &u.State, &u.FailureReason, &u.FailureDetail, &u.ClosedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.StartedAt = mustTime(startedAt)
	u.UpdatedAt = mustTime(updatedAt)
	if u.UpdatedAt.IsZero() {
		u.UpdatedAt = u.StartedAt
	}
	u.FinishedAt = parseTime(finishedAt)
	u.Stalled = u.IsStalled(time.Now())
	return &u, nil
}

// CreateUpgrade inserts a new upgrade in state requested, with its first
// timeline event, and returns its id.
func (s *Store) CreateUpgrade(ctx context.Context, deviceID int64, fromVersion, toVersion, requestedBy string) (string, error) {
	id := ulid.Make().String()
	now := nowString()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agent_upgrades (id, device_id, from_version, to_version, requested_by, started_at, updated_at, state)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, deviceID, fromVersion, toVersion, requestedBy, now, now, proto.UpgradeRequested); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agent_upgrade_events (upgrade_id, ts, source, state) VALUES (?, ?, ?, ?)`,
		id, now, UpgradeSourceServer, proto.UpgradeRequested); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// RecordUpgradeEvent appends ev to its upgrade's timeline and, when the
// rules below allow it, moves the upgrade to ev.State. It reports whether the
// event was applied.
//
//   - An in-flight upgrade takes any valid state. An event repeating the
//     current state is an annotation: it adds a timeline line and leaves the
//     row, updated_at included, as it was.
//   - A terminal upgrade closed by the server or an admin takes a terminal
//     state from the agent: the agent's own report supersedes an inferred or
//     abandoned outcome.
//   - Anything else is stored with applied = 0 and the row is untouched, so
//     a late report can never put a closed upgrade back in flight.
//
// Once an upgrade has MaxUpgradeEvents events, further ones are refused with
// ErrTooManyEvents. The exception is a server or admin event that changes
// the row, so that a flood of agent reports cannot make an upgrade
// impossible to abandon or time out.
func (s *Store) RecordUpgradeEvent(ctx context.Context, ev UpgradeEvent) (applied bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	u, err := scanUpgrade(tx.QueryRowContext(ctx, upgradeSelect+`WHERE u.id = ?`, ev.UpgradeID))
	if err != nil {
		return false, err
	}
	applied, err = recordUpgradeEvent(ctx, tx, u, ev)
	if err != nil {
		return false, err
	}
	return applied, tx.Commit()
}

func recordUpgradeEvent(ctx context.Context, tx *sql.Tx, u *Upgrade, ev UpgradeEvent) (bool, error) {
	if !proto.ValidUpgradeState(ev.State) {
		return false, fmt.Errorf("%w: state %q", ErrInvalidUpgradeEvent, ev.State)
	}
	switch ev.Source {
	case UpgradeSourceAgent, UpgradeSourceServer, UpgradeSourceAdmin:
	default:
		return false, fmt.Errorf("%w: source %q", ErrInvalidUpgradeEvent, ev.Source)
	}
	terminal := isTerminalUpgradeState(ev.State)
	// applied is what the timeline shows; change is whether the row moves.
	var applied, change bool
	switch {
	case u.IsInFlight():
		applied, change = true, ev.State != u.State
	case terminal && ev.Source == UpgradeSourceAgent &&
		(u.ClosedBy == UpgradeSourceServer || u.ClosedBy == UpgradeSourceAdmin):
		applied, change = true, true
	}
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agent_upgrade_events WHERE upgrade_id = ?`, u.ID).Scan(&n); err != nil {
		return false, err
	}
	if n >= MaxUpgradeEvents && !(change && ev.Source != UpgradeSourceAgent) {
		return false, ErrTooManyEvents
	}
	ts := nowString()
	if !ev.TS.IsZero() {
		ts = timeString(ev.TS)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO agent_upgrade_events (upgrade_id, ts, source, state, reason, detail, applied) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		u.ID, ts, ev.Source, ev.State, ev.Reason, ev.Detail, b2i(applied)); err != nil {
		return false, err
	}
	if !change {
		return applied, nil
	}
	var finishedAt any
	closedBy := ""
	if terminal {
		finishedAt, closedBy = ts, ev.Source
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE agent_upgrades SET state = ?, failure_reason = ?, failure_detail = ?, updated_at = ?,
		 finished_at = ?, closed_by = ? WHERE id = ?`,
		ev.State, ev.Reason, ev.Detail, ts, finishedAt, closedBy, u.ID)
	return applied, err
}

// GetActiveUpgrade returns the in-flight upgrade holding the fleet's one
// slot, oldest first if there is ever more than one, or nil.
func (s *Store) GetActiveUpgrade(ctx context.Context) (*Upgrade, error) {
	u, err := scanUpgrade(s.db.QueryRowContext(ctx,
		upgradeSelect+`WHERE u.state NOT IN (?, ?, ?) ORDER BY u.started_at, u.rowid LIMIT 1`,
		proto.UpgradeVerified, proto.UpgradeRolledBack, proto.UpgradeFailed))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return u, err
}

// GetLatestUpgrade returns the device's newest upgrade, or nil.
func (s *Store) GetLatestUpgrade(ctx context.Context, deviceID int64) (*Upgrade, error) {
	u, err := scanUpgrade(s.db.QueryRowContext(ctx,
		upgradeSelect+`WHERE u.device_id = ? ORDER BY u.started_at DESC, u.rowid DESC LIMIT 1`, deviceID))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return u, err
}

// GetUpgrade looks an upgrade up by id.
func (s *Store) GetUpgrade(ctx context.Context, id string) (*Upgrade, error) {
	return scanUpgrade(s.db.QueryRowContext(ctx, upgradeSelect+`WHERE u.id = ?`, id))
}

// FindUpgradeForResult picks the upgrade an agent's result belongs to. The
// device id always filters, so a device can only reach its own rows. With an
// upgradeID it is the row with that id; without one (an agent that does not
// echo it) it is the device's newest row for toVersion, in-flight rows
// first. No match returns ErrNotFound.
func (s *Store) FindUpgradeForResult(ctx context.Context, deviceID int64, upgradeID, toVersion string) (*Upgrade, error) {
	if upgradeID != "" {
		return scanUpgrade(s.db.QueryRowContext(ctx,
			upgradeSelect+`WHERE u.id = ? AND u.device_id = ?`, upgradeID, deviceID))
	}
	return scanUpgrade(s.db.QueryRowContext(ctx,
		upgradeSelect+`WHERE u.device_id = ? AND u.to_version = ?
		 ORDER BY (u.state IN (?, ?, ?)), u.started_at DESC, u.rowid DESC LIMIT 1`,
		deviceID, toVersion, proto.UpgradeVerified, proto.UpgradeRolledBack, proto.UpgradeFailed))
}

// GetDeviceUpgrades returns up to limit of the device's upgrades, newest
// first. A limit of zero or less means no limit.
func (s *Store) GetDeviceUpgrades(ctx context.Context, deviceID int64, limit int) ([]Upgrade, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := s.db.QueryContext(ctx,
		upgradeSelect+`WHERE u.device_id = ? ORDER BY u.started_at DESC, u.rowid DESC LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Upgrade
	for rows.Next() {
		u, err := scanUpgrade(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// ListUpgradeEvents returns an upgrade's timeline, oldest first. Upgrades
// that predate the events table have none.
func (s *Store) ListUpgradeEvents(ctx context.Context, id string) ([]UpgradeEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT upgrade_id, ts, source, state, reason, detail, applied
		 FROM agent_upgrade_events WHERE upgrade_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UpgradeEvent{}
	for rows.Next() {
		var e UpgradeEvent
		var ts string
		var applied int
		if err := rows.Scan(&e.UpgradeID, &ts, &e.Source, &e.State, &e.Reason, &e.Detail, &applied); err != nil {
			return nil, err
		}
		e.TS = mustTime(ts)
		e.Applied = applied == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) HasFailedUpgrade(ctx context.Context, deviceID int64, toVersion string) bool {
	var n int
	s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agent_upgrades WHERE device_id=? AND to_version=? AND state IN (?, ?)`,
		deviceID, toVersion, proto.UpgradeRolledBack, proto.UpgradeFailed).Scan(&n)
	return n > 0
}

// AbandonUpgrade closes an in-flight upgrade of the given device as
// failed/abandoned. It returns ErrNotFound if the id is unknown or belongs
// to another device, and ErrUpgradeNotInFlight if the upgrade already ended.
// It does not stop the agent; a later terminal report from it supersedes the
// abandon.
func (s *Store) AbandonUpgrade(ctx context.Context, deviceID int64, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	u, err := scanUpgrade(tx.QueryRowContext(ctx,
		upgradeSelect+`WHERE u.id = ? AND u.device_id = ?`, id, deviceID))
	if err != nil {
		return err
	}
	if !u.IsInFlight() {
		return ErrUpgradeNotInFlight
	}
	if _, err := recordUpgradeEvent(ctx, tx, u, UpgradeEvent{
		UpgradeID: id, Source: UpgradeSourceAdmin, State: proto.UpgradeFailed, Reason: UpgradeReasonAbandoned,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// Upgrade dispatch codes: what came of the server's last attempt to offer a
// device its desired version.
const (
	DispatchSent         = "sent"
	DispatchInFlight     = "in_flight"
	DispatchPrevFailed   = "prev_failed"
	DispatchNoRelease    = "no_release"
	DispatchNotConnected = "not_connected"
	DispatchSendFailed   = "send_failed"
	DispatchWaiting      = "waiting"
)

// UpgradeDispatch is the outcome of the last upgrade evaluation for a
// device, kept so the dashboard can say why nothing was sent.
type UpgradeDispatch struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// BlockingDevice holds the fleet's one upgrade slot; set for in_flight.
	BlockingDevice string `json:"blocking_device"`
	// UpgradeID is the upgrade created by this dispatch; set for sent.
	UpgradeID string    `json:"upgrade_id"`
	At        time.Time `json:"at"`
	// BlockingState is the blocking upgrade's state and BlockingSince when
	// that state began (its updated_at). Both are set only for in_flight.
	BlockingState string     `json:"blocking_state,omitempty"`
	BlockingSince *time.Time `json:"blocking_since,omitempty"`
}

// SetUpgradeDispatch stores the device's dispatch outcome. A zero Code
// clears it.
func (s *Store) SetUpgradeDispatch(ctx context.Context, deviceID int64, disp UpgradeDispatch) error {
	body := ""
	if disp.Code != "" {
		b, err := json.Marshal(disp)
		if err != nil {
			return err
		}
		body = string(b)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET upgrade_dispatch_json = ? WHERE id = ?`, body, deviceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetDesiredAgentVersion(ctx context.Context, name, version string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET desired_agent_version=? WHERE name=?`, version, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
