package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/jhyoong/KumaBoard/server/store"
)

// runDeviceAdd registers a device from the command line and prints its token once.
func runDeviceAdd(args []string) error {
	fs := flag.NewFlagSet("device add", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to config.yaml")
	mac := fs.String("mac", "", "MAC address for Wake-on-LAN (optional)")
	normallyOff := fs.Bool("normally-off", false, "device is expected to be offline by default")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kumaboard device add [-config PATH] [-mac MAC] [-normally-off] NAME")
	}
	cfg, err := loadConfigFlag([]string{"-config", *cfgPath})
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DataDir + "/kumaboard.db")
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	d, token, err := st.CreateDevice(ctx, fs.Arg(0), *mac, *normallyOff, store.Schedule{})
	if err != nil {
		return err
	}
	st.Audit(ctx, "cli", "device_add", d.Name, "ok", "")
	fmt.Printf("device %s registered\ntoken (shown once):\n%s\n", d.Name, token)
	return nil
}
