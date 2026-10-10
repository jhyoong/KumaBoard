//go:build windows

package upgrade

import (
	"fmt"
	"os"
)

func Swap(currentPath, tempPath string) error {
	oldPath := currentPath + ".old"
	os.Remove(oldPath)
	CleanupFailed(currentPath)
	if err := os.Rename(currentPath, oldPath); err != nil {
		return fmt.Errorf("swap: rename current to old: %w", err)
	}
	if err := os.Rename(tempPath, currentPath); err != nil {
		os.Rename(oldPath, currentPath)
		return fmt.Errorf("swap: rename new to current: %w", err)
	}
	return nil
}

// RestoreOld puts the previous binary back. The running exe cannot be
// replaced in place, so it is moved aside first; every failure leaves the
// running exe at currentPath.
func RestoreOld(currentPath string) error {
	oldPath := currentPath + ".old"
	if _, err := os.Stat(oldPath); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	failedPath := currentPath + ".failed"
	os.Remove(failedPath)
	if err := os.Rename(currentPath, failedPath); err != nil {
		return fmt.Errorf("restore: move running binary: %w", err)
	}
	if err := os.Rename(oldPath, currentPath); err != nil {
		if uerr := os.Rename(failedPath, currentPath); uerr != nil {
			return fmt.Errorf("restore: move old back: %w; undo failed: %v", err, uerr)
		}
		return fmt.Errorf("restore: move old back: %w", err)
	}
	return nil
}

// syncDir is a no-op: Windows cannot flush a directory handle.
func syncDir(string) {}

func CleanupFailed(currentPath string) {
	os.Remove(currentPath + ".failed")
}
