//go:build windows

package main

import (
	"fmt"

	"github.com/jhyoong/KumaBoard/agent/terminal"
)

// selftestPTYPlatform reports that terminals are unsupported rather than
// failing: the selftest gates upgrades, and terminal_open is refused anyway.
func selftestPTYPlatform() error {
	fmt.Printf("pty: unsupported (%s); terminal_open will be refused\n", terminal.UnsupportedReason)
	return nil
}
