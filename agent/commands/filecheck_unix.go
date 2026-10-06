//go:build !windows

package commands

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// notWritable fails when this process could modify p. Both halves matter:
// an owner can always chmod its way back in, and under a read-only mount
// (systemd ProtectSystem=strict) the access probe reports EROFS even for a
// file the agent owns, so ownership is what still catches that. As root
// every probe succeeds, so the rule becomes: no group or world write bit.
func (c *fileChecker) notWritable(p string, fi os.FileInfo) error {
	euid := os.Geteuid()
	if c.euid != nil {
		euid = *c.euid
	}
	if euid == 0 {
		if fi.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("%s is group- or world-writable", p)
		}
		return nil
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: owner unknown", p)
	}
	if int(st.Uid) == euid {
		return fmt.Errorf("%s is owned by the agent account", p)
	}
	if unix.Faccessat(unix.AT_FDCWD, p, unix.W_OK, unix.AT_EACCESS) == nil {
		return fmt.Errorf("%s is writable by the agent account", p)
	}
	return nil
}
