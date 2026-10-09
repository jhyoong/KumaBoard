package upgrade

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/jhyoong/KumaBoard/proto"
)

// OutboxFile holds one terminal upgrade result that could not be sent. It
// lives in the binary directory and is sent, then deleted, on the next
// connect.
const OutboxFile = "upgrade_report.json"

// outboxMu serializes a write from the upgrade goroutine against a flush
// from the connection goroutine.
var outboxMu sync.Mutex

// WriteOutbox stores res for FlushOutbox, replacing any earlier result.
func WriteOutbox(binaryDir string, res proto.UpgradeResult) error {
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	outboxMu.Lock()
	defer outboxMu.Unlock()
	return os.WriteFile(filepath.Join(binaryDir, OutboxFile), b, 0o600)
}

// ReadOutbox returns the stored result, or nil when there is none.
func ReadOutbox(binaryDir string) (*proto.UpgradeResult, error) {
	b, err := os.ReadFile(filepath.Join(binaryDir, OutboxFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var res proto.UpgradeResult
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// FlushOutbox sends the stored result, if any, and deletes it once send
// returns nil. A result that cannot be sent stays for the next connect; a
// file that cannot be decoded is deleted. It reports whether a result was
// delivered.
func FlushOutbox(binaryDir string, send Sender) (bool, error) {
	outboxMu.Lock()
	defer outboxMu.Unlock()
	path := filepath.Join(binaryDir, OutboxFile)
	res, err := ReadOutbox(binaryDir)
	if err != nil {
		os.Remove(path)
		return false, err
	}
	if res == nil {
		return false, nil
	}
	if err := send(*res); err != nil {
		return false, err
	}
	return true, os.Remove(path)
}
