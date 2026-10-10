package upgrade

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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
	// Temp file, fsync, rename: power loss must leave the old marker or the
	// new one, never a truncated file the next start cannot parse.
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
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
