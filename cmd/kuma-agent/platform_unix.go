//go:build !windows

package main

const platformDefaultConfig = "/etc/kuma-agent/config.yaml"

func maybeRunService() bool { return false }

func runInstall(args []string) error   { return errNotWindows }
func runUninstall(args []string) error { return errNotWindows }
