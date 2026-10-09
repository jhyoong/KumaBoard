package upgrade

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"
)

const PendingFile = "upgrade_pending.json"

// Pending is the marker left beside the binary across an upgrade restart. It
// is written by one binary and read by the other, so every field added after
// RollbackReason is optional JSON.
type Pending struct {
	FromVersion    string    `json:"from_version"`
	ToVersion      string    `json:"to_version"`
	StartedAt      time.Time `json:"started_at"`
	RollbackReason string    `json:"rollback_reason"`
	UpgradeID      string    `json:"upgrade_id,omitempty"`
	RollbackDetail string    `json:"rollback_detail,omitempty"`
	Starts         int       `json:"starts,omitempty"` // probation starts so far
}

func ReadPending(path string) (*Pending, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Pending
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func WritePending(path string, p *Pending) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// SetRollback records why the new binary is being rolled back. A missing
// marker is an error: without it the restored binary cannot report anything.
func SetRollback(path, reason, detail string) error {
	p, err := ReadPending(path)
	if err != nil {
		return err
	}
	if p == nil {
		return fs.ErrNotExist
	}
	p.RollbackReason = reason
	p.RollbackDetail = detail
	return WritePending(path, p)
}
