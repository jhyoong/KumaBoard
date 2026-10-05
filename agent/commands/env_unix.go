//go:build !windows

package commands

import (
	"os"
	"os/exec"
	"syscall"
)

func fixedEnv() []string {
	return []string{
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + os.Getenv("HOME"),
		"LANG=C.UTF-8",
	}
}

func setProcAttr(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
}
