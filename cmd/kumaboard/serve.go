package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jhyoong/KumaBoard/internal/buildinfo"
	"github.com/jhyoong/KumaBoard/server/api"
	"github.com/jhyoong/KumaBoard/server/auth"
	"github.com/jhyoong/KumaBoard/server/config"
	"github.com/jhyoong/KumaBoard/server/hub"
	"github.com/jhyoong/KumaBoard/server/pki"
	"github.com/jhyoong/KumaBoard/server/registry"
	"github.com/jhyoong/KumaBoard/server/sse"
	"github.com/jhyoong/KumaBoard/server/store"
	"github.com/jhyoong/KumaBoard/server/terminal"
	"github.com/jhyoong/KumaBoard/server/wake"
	"golang.org/x/term"
)

const defaultConfigPath = "/etc/kumaboard/config.yaml"

func loadConfigFlag(args []string) (*config.Config, error) {
	fs := flag.NewFlagSet("kumaboard", flag.ContinueOnError)
	path := fs.String("config", defaultConfigPath, "path to config.yaml")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return config.Load(*path)
}

func runServe(args []string) error {
	cfg, err := loadConfigFlag(args)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	pkiDir := filepath.Join(cfg.DataDir, "pki")
	if err := pki.Ensure(pkiDir, cfg.SANs()); err != nil {
		return err
	}
	if exp, err := pki.ServerCertExpiry(pkiDir); err == nil && time.Until(exp) < 60*24*time.Hour {
		log.Warn("server certificate expires soon; run: kumaboard cert reissue", "expires", exp)
	}
	tlsCfg, err := pki.TLSConfig(pkiDir)
	if err != nil {
		return err
	}

	st, err := store.Open(filepath.Join(cfg.DataDir, "kumaboard.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	if n, err := st.MarkAllInFlightLost(context.Background()); err != nil {
		return err
	} else if n > 0 {
		log.Info("marked in-flight runs lost at startup", "count", n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		// Metrics history: 30s samples back the 1h card view and are folded
		// into 15-minute rollups after 2h; rollups are kept for 30 days.
		prune := time.NewTicker(10 * time.Minute)
		defer prune.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				st.DeleteExpiredSessions(ctx)
			case <-prune.C:
				now := time.Now()
				if err := st.RollupMetrics(ctx, now); err != nil {
					log.Warn("rollup metrics history", "err", err)
				}
				if _, err := st.DeleteMetricsBefore(ctx, now.Add(-store.MetricsRetention)); err != nil {
					log.Warn("prune metrics history", "err", err)
				}
			}
		}
	}()

	handler, h, err := buildHandler(cfg, st, log)
	if err != nil {
		return err
	}
	go func() {
		// Upgrades that stopped reporting: timed out before the swap,
		// flagged as stalled after it.
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				h.SweepUpgrades(ctx, now)
			}
		}
	}()

	srv := &http.Server{
		Handler:           handler,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, len(cfg.ListenAddrs))
	for _, addr := range cfg.ListenAddrs {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", addr, err)
		}
		log.Info("listening", "addr", addr, "version", buildinfo.Version)
		go func() { errCh <- srv.ServeTLS(ln, "", "") }()
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-stop:
		log.Info("shutting down", "signal", sig)
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	return srv.Shutdown(shutCtx)
}

func buildHandler(cfg *config.Config, st *store.Store, log *slog.Logger) (http.Handler, *hub.Hub, error) {
	broker := sse.New()
	reg := registry.New(st, broker, time.Duration(cfg.MetricsIntervalS)*time.Second)
	if err := reg.Load(context.Background()); err != nil {
		return nil, nil, err
	}
	go reg.Run(context.Background())

	termBroker := terminal.NewBroker(st, log)
	h := hub.New(hub.Options{
		Store:             st,
		Events:            reg,
		Log:               log,
		MetricsInterval:   time.Duration(cfg.MetricsIntervalS) * time.Second,
		HeartbeatInterval: time.Duration(cfg.HeartbeatIntervalS) * time.Second,
		TerminalRefused:   func(dev, sid string) { termBroker.RefuseSession(dev, sid) },
	})

	var wakeFn api.WakeFunc
	if cfg.WOL.Interface != "" && cfg.WOL.Broadcast != "" {
		sender, err := wake.NewSender(cfg.WOL.Interface, cfg.WOL.Broadcast)
		if err != nil {
			log.Warn("wake-on-lan disabled", "err", err)
		} else {
			wakeFn = sender.Send
			log.Info("wake-on-lan ready", "source", sender.SourceIP, "broadcast", cfg.WOL.Broadcast)
		}
	}

	allowed := cfg.AllowedHosts()
	apiHandler := api.New(api.Deps{
		Store: st, Registry: reg, Hub: h, Broker: broker,
		Sessions: auth.NewSessions(st), Limiter: auth.NewLimiter(5, time.Minute),
		Terminal: termBroker, AllowedHosts: allowed, Log: log, Wake: wakeFn,
		ReleasesDir: filepath.Join(cfg.DataDir, "releases"),
	})

	mux := http.NewServeMux()
	mux.Handle("GET /ws", auth.OriginCheck(allowed)(h))
	mux.Handle("GET /ws/terminal", auth.OriginCheck(allowed)(termBroker))
	mux.Handle("/api/", apiHandler)
	mux.Handle("/", api.Static())
	return auth.HostCheck(allowed)(mux), h, nil
}

func runPasswd(args []string) error {
	fs := flag.NewFlagSet("passwd", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to config.yaml")
	user := fs.String("user", "admin", "operator username")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	fmt.Fprint(os.Stderr, "New password: ")
	p1, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	fmt.Fprint(os.Stderr, "Repeat password: ")
	p2, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	defer func() {
		for i := range p1 {
			p1[i] = 0
		}
		for i := range p2 {
			p2[i] = 0
		}
	}()
	if string(p1) != string(p2) {
		return errors.New("passwords do not match")
	}
	if len(p1) < 12 {
		return errors.New("password must be at least 12 characters")
	}
	hash, err := auth.HashPassword(string(p1))
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "kumaboard.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.UpsertUser(context.Background(), *user, hash); err != nil {
		return err
	}
	st.Audit(context.Background(), "cli", "passwd", *user, "ok", "")
	fmt.Printf("password set for %s\n", *user)
	return nil
}

func runCertReissue(args []string) error {
	cfg, err := loadConfigFlag(args)
	if err != nil {
		return err
	}
	dir := filepath.Join(cfg.DataDir, "pki")
	if err := pki.Reissue(dir, cfg.SANs()); err != nil {
		return err
	}
	fmt.Println("server certificate reissued; restart kumaboard to load it")
	return nil
}
