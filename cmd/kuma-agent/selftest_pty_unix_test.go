//go:build !windows

package main

import (
	"os"
	"testing"
)

func TestSelftestPTYPlatform(t *testing.T) {
	if f, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0); err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	} else {
		f.Close()
	}
	t.Setenv("SHELL", "/bin/sh")
	if err := selftestPTYPlatform(); err != nil {
		t.Fatal(err)
	}
}
