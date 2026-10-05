package upgrade

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"
)

const PendingFile = "upgrade_pending.json"

type Pending struct {
	FromVersion    string    `json:"from_version"`
	ToVersion      string    `json:"to_version"`
	StartedAt      time.Time `json:"started_at"`
	RollbackReason string    `json:"rollback_reason"`
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

func SetRollbackReason(path, reason string) error {
	p, err := ReadPending(path)
	if err != nil || p == nil {
		return err
	}
	p.RollbackReason = reason
	return WritePending(path, p)
}
