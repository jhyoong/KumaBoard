//go:build !windows

package upgrade

import (
	"fmt"
	"os"
)

func Swap(currentPath, tempPath string) error {
	oldPath := currentPath + ".old"
	os.Remove(oldPath)
	if err := os.Link(currentPath, oldPath); err != nil {
		if err2 := os.Rename(currentPath, oldPath); err2 != nil {
			return fmt.Errorf("swap: backup current: %w", err2)
		}
	}
	if err := os.Rename(tempPath, currentPath); err != nil {
		os.Rename(oldPath, currentPath)
		return fmt.Errorf("swap: rename new to current: %w", err)
	}
	return nil
}

func RestoreOld(currentPath string) error {
	oldPath := currentPath + ".old"
	if err := os.Rename(oldPath, currentPath); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	return nil
}

func CleanupFailed(currentPath string) {}
