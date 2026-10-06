//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
	"golang.org/x/term"

	"github.com/jhyoong/KumaBoard/agent/config"
)

const (
	platformDefaultConfig = `C:\ProgramData\kuma-agent\config.yaml`
	serviceName           = "kuma-agent"
	logPath               = `C:\ProgramData\kuma-agent\agent.log`
)

func maybeRunService() bool {
	isSvc, err := svc.IsWindowsService()
	if err != nil || !isSvc {
		return false
	}
	svc.Run(serviceName, &service{cfgPath: defaultConfigPath()})
	return true
}

type service struct{ cfgPath string }

func (s *service) Execute(args []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	cfg, err := config.Load(s.cfgPath)
	if err != nil {
		// The log is normally opened after the config; open it here too so
		// this failure, and a rollback it triggers, leave a trace.
		log := slog.New(slog.DiscardHandler)
		if f, ferr := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); ferr == nil {
			defer f.Close()
			log = slog.New(slog.NewTextHandler(f, nil))
		}
		log.Error("cannot load config", "path", s.cfgPath, "err", err)
		rollbackFailedStartup(err, log)
		return true, 1
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return true, 2
	}
	defer logFile.Close()
	log := slog.New(slog.NewTextHandler(logFile, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runWithConfig(ctx, cfg, log)
		close(done)
	}()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for r := range req {
		switch r.Cmd {
		case svc.Interrogate:
			status <- r.CurrentStatus
		case svc.Stop, svc.Shutdown:
			status <- svc.Status{State: svc.StopPending}
			cancel()
			<-done
			return false, 0
		}
	}
	// The service manager never closes req, but if it ever did, shut the
	// agent down the same way a Stop request would.
	cancel()
	<-done
	return false, 0
}

func runInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	account := fs.String("account", `.\kuma-agent`, "service logon account")
	password := fs.String("password", "", "account password (prompted when empty)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *password == "" {
		fmt.Fprintf(os.Stderr, "Password for %s: ", *account)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		*password = string(b)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (run as administrator): %w", err)
	}
	defer m.Disconnect()
	if s, err := m.OpenService(serviceName); err == nil {
		s.Close()
		return errors.New("service already installed; run uninstall first")
	}
	s, err := m.CreateService(serviceName, exe, mgr.Config{
		DisplayName:      "KumaBoard Agent",
		Description:      "Connects this device to the KumaBoard control plane.",
		StartType:        mgr.StartAutomatic,
		ServiceStartName: *account,
		Password:         *password,
	})
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()
	restart := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: 5 * time.Second}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{restart, restart, restart}, 86400); err != nil {
		return fmt.Errorf("set recovery actions: %w", err)
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("set recovery on non-crash failures: %w", err)
	}
	fmt.Println("service installed; start it with: sc start kuma-agent")
	return nil
}

func runUninstall(args []string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return errors.New("service is not installed")
	}
	defer s.Close()
	if st, err := s.Query(); err == nil && st.State != svc.Stopped {
		s.Control(svc.Stop)
		time.Sleep(2 * time.Second)
	}
	if err := s.Delete(); err != nil {
		return err
	}
	fmt.Println("service removed")
	return nil
}
