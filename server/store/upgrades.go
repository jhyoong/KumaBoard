package store

import (
	"context"
	"database/sql"
	"errors"
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

type Upgrade struct {
	ID            string     `json:"id"`
	DeviceID      int64      `json:"device_id"`
	FromVersion   string     `json:"from_version"`
	ToVersion     string     `json:"to_version"`
	RequestedBy   string     `json:"requested_by"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
	State         string     `json:"state"`
	FailureReason string     `json:"failure_reason"`
}

func (u *Upgrade) IsInFlight() bool {
	switch u.State {
	case proto.UpgradeVerified, proto.UpgradeRolledBack, proto.UpgradeFailed:
		return false
	}
	return true
}

func (s *Store) CreateUpgrade(ctx context.Context, deviceID int64, fromVersion, toVersion, requestedBy string) (string, error) {
	id := ulid.Make().String()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_upgrades (id, device_id, from_version, to_version, requested_by, started_at, state)
		 VALUES (?, ?, ?, ?, ?, ?, 'requested')`,
		id, deviceID, fromVersion, toVersion, requestedBy, nowString())
	return id, err
}

func (s *Store) UpdateUpgradeState(ctx context.Context, id, state, reason string) error {
	var finishedAt any
	switch state {
	case proto.UpgradeVerified, proto.UpgradeRolledBack, proto.UpgradeFailed:
		now := nowString()
		finishedAt = now
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE agent_upgrades SET state=?, failure_reason=?, finished_at=COALESCE(?, finished_at) WHERE id=?`,
		state, reason, finishedAt, id)
	return err
}

func (s *Store) GetActiveUpgrade(ctx context.Context) (*Upgrade, error) {
	var u Upgrade
	var startedAt string
	var finishedAt sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, device_id, from_version, to_version, requested_by, started_at, finished_at, state, failure_reason
		 FROM agent_upgrades
		 WHERE state NOT IN (?, ?, ?)
		 LIMIT 1`,
		proto.UpgradeVerified, proto.UpgradeRolledBack, proto.UpgradeFailed,
	).Scan(&u.ID, &u.DeviceID, &u.FromVersion, &u.ToVersion, &u.RequestedBy,
		&startedAt, &finishedAt, &u.State, &u.FailureReason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.StartedAt = mustTime(startedAt)
	u.FinishedAt = parseTime(finishedAt)
	return &u, nil
}

func (s *Store) GetLatestUpgrade(ctx context.Context, deviceID int64) (*Upgrade, error) {
	var u Upgrade
	var startedAt string
	var finishedAt sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, device_id, from_version, to_version, requested_by, started_at, finished_at, state, failure_reason
		 FROM agent_upgrades WHERE device_id=? ORDER BY started_at DESC LIMIT 1`,
		deviceID).Scan(&u.ID, &u.DeviceID, &u.FromVersion, &u.ToVersion, &u.RequestedBy,
		&startedAt, &finishedAt, &u.State, &u.FailureReason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.StartedAt = mustTime(startedAt)
	u.FinishedAt = parseTime(finishedAt)
	return &u, nil
}

func (s *Store) GetDeviceUpgrades(ctx context.Context, deviceID int64) ([]Upgrade, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, device_id, from_version, to_version, requested_by, started_at, finished_at, state, failure_reason
		 FROM agent_upgrades WHERE device_id=? ORDER BY started_at DESC`,
		deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Upgrade
	for rows.Next() {
		var u Upgrade
		var startedAt string
		var finishedAt sql.NullString
		if err := rows.Scan(&u.ID, &u.DeviceID, &u.FromVersion, &u.ToVersion, &u.RequestedBy,
			&startedAt, &finishedAt, &u.State, &u.FailureReason); err != nil {
			return nil, err
		}
		u.StartedAt = mustTime(startedAt)
		u.FinishedAt = parseTime(finishedAt)
		out = append(out, u)
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

func (s *Store) AbandonUpgrade(ctx context.Context, id string) error {
	return s.UpdateUpgradeState(ctx, id, proto.UpgradeFailed, "abandoned")
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
