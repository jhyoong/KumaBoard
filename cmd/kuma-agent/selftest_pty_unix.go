//go:build !windows

package main

import (
	"fmt"
	"time"

	"github.com/jhyoong/KumaBoard/agent/terminal"
)

// selftestPTYPlatform runs the same spawn path as a live terminal session.
func selftestPTYPlatform() error {
	if err := terminal.SelfTest(10 * time.Second); err != nil {
		return err
	}
	fmt.Println("pty: ok")
	return nil
}
