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

func RestoreOld(currentPath string) error {
	failedPath := currentPath + ".failed"
	os.Remove(failedPath)
	if err := os.Rename(currentPath, failedPath); err != nil {
		return fmt.Errorf("restore: move running binary: %w", err)
	}
	oldPath := currentPath + ".old"
	if err := os.Rename(oldPath, currentPath); err != nil {
		return fmt.Errorf("restore: move old back: %w", err)
	}
	return nil
}

func CleanupFailed(currentPath string) {
	os.Remove(currentPath + ".failed")
}
