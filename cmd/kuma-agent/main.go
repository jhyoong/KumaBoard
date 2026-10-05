// Command kuma-agent connects a device to the KumaBoard control plane.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/jhyoong/KumaBoard/agent/app"
	"github.com/jhyoong/KumaBoard/agent/collectors"
	"github.com/jhyoong/KumaBoard/agent/config"
	"github.com/jhyoong/KumaBoard/agent/transport"
	"github.com/jhyoong/KumaBoard/agent/upgrade"
	"github.com/jhyoong/KumaBoard/internal/buildinfo"
	"github.com/jhyoong/KumaBoard/proto"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage: kuma-agent <command> [flags]

commands:
  run        connect to the control plane (flags: -config PATH)
  install    install the Windows service (Windows only)
  selftest   run local checks and exit (flags: -config PATH)
  uninstall  remove the Windows service (Windows only)
  version    print version and protocol version`)
	os.Exit(2)
}

func defaultConfigPath() string {
	if p := os.Getenv("KUMA_AGENT_CONFIG"); p != "" {
		return p
	}
	return platformDefaultConfig
}

func main() {
	if maybeRunService() {
		return
	}
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runAgent(os.Args[2:])
	case "selftest":
		err = runSelftest(os.Args[2:])
	case "install":
		err = runInstall(os.Args[2:])
	case "uninstall":
		err = runUninstall(os.Args[2:])
	case "version":
		fmt.Printf("kuma-agent %s protocol %d\n", buildinfo.Version, proto.Version)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func runAgent(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	path := fs.String("config", defaultConfigPath(), "path to config.yaml")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runWithConfig(ctx, cfg, log)
}

func runSelftest(args []string) error {
	fs := flag.NewFlagSet("selftest", flag.ContinueOnError)
	path := fs.String("config", defaultConfigPath(), "path to config.yaml")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return fmt.Errorf("selftest: config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The GPU probe runs too, but GPU errors never fail Collect, so a flaky GPU
	// cannot block an upgrade.
	gpu := slices.Contains(cfg.Capabilities, proto.CapGPU) && slices.Contains(cfg.Capabilities, proto.CapMetrics)
	if _, err := collectors.New(collectors.Options{GPU: gpu}).Collect(ctx); err != nil {
		return fmt.Errorf("selftest: collectors: %w", err)
	}
	for _, cap := range cfg.Capabilities {
		if cap == proto.CapTerminal {
			if err := selftestPTYPlatform(); err != nil {
				return fmt.Errorf("selftest: pty: %w", err)
			}
			break
		}
	}
	fmt.Printf("kuma-agent %s protocol %d\n", buildinfo.Version, proto.Version)
	return nil
}

// runWithConfig is shared by the foreground command and the Windows service.
func runWithConfig(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine executable path: %w", err)
	}
	startupResult := upgrade.CheckStartup(filepath.Dir(exe), log)
	a := app.New(cfg, log, startupResult)
	client := &transport.Client{
		URL:     cfg.Server.URL,
		TLS:     &tls.Config{RootCAs: cfg.CAPool, MinVersion: tls.VersionTLS12},
		Hello:   a.Hello,
		Handler: a,
		Log:     log,
	}
	log.Info("kuma-agent starting", "device", cfg.Device.Name, "version", buildinfo.Version, "server", cfg.Server.URL)
	client.Run(ctx)
	// Terminal sessions outlive control reconnects, so end them explicitly
	// on shutdown and give their shells a moment to be killed.
	a.Close(10 * time.Second)
	return nil
}
