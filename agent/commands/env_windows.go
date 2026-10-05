//go:build windows

package commands

import (
	"os"
	"os/exec"
)

func fixedEnv() []string {
	root := os.Getenv("SYSTEMROOT")
	if root == "" {
		root = `C:\Windows`
	}
	return []string{
		"PATH=" + root + `\System32;` + root + `;` + root + `\System32\WindowsPowerShell\v1.0`,
		"SYSTEMROOT=" + root,
		"TEMP=" + os.Getenv("TEMP"),
		"TMP=" + os.Getenv("TMP"),
		"USERPROFILE=" + os.Getenv("USERPROFILE"),
	}
}

func setProcAttr(c *exec.Cmd) {}
