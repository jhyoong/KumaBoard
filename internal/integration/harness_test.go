// Package integration starts a real server and real agents in one process.
package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/agent/app"
	"github.com/jhyoong/KumaBoard/agent/config"
	"github.com/jhyoong/KumaBoard/agent/transport"
	"github.com/jhyoong/KumaBoard/agent/upgrade"
	"github.com/jhyoong/KumaBoard/internal/buildinfo"
	"github.com/jhyoong/KumaBoard/internal/signing"
	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/api"
	"github.com/jhyoong/KumaBoard/server/auth"
	"github.com/jhyoong/KumaBoard/server/hub"
	"github.com/jhyoong/KumaBoard/server/pki"
	"github.com/jhyoong/KumaBoard/server/registry"
	"github.com/jhyoong/KumaBoard/server/sse"
	"github.com/jhyoong/KumaBoard/server/store"
	"github.com/jhyoong/KumaBoard/server/terminal"
)

type recorder struct {
	mu     sync.Mutex
	events []string
	output []recordedOutput
}

type recordedOutput struct {
	runID string
	seq   int
	o     proto.CommandOutput
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, s)
}
func (r *recorder) DeviceConnected(name string)                  { r.add("connect:" + name) }
func (r *recorder) DeviceDisconnected(name string)               { r.add("disconnect:" + name) }
func (r *recorder) MetricsReceived(name string, m proto.Metrics) { r.add("metrics:" + name) }
func (r *recorder) RunChanged(runID string)                      { r.add("run:" + runID) }
func (r *recorder) UpgradeChanged(name string)                   { r.add("upgrade:" + name) }
func (r *recorder) CommandsChanged(name string)                  { r.add("commands:" + name) }
func (r *recorder) RunOutput(runID, device string, seq int, o proto.CommandOutput) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.output = append(r.output, recordedOutput{runID, seq, o})
}
func (r *recorder) count(s string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e == s {
			n++
		}
	}
	return n
}

type harness struct {
	t      *testing.T
	dir    string
	st     *store.Store
	hub    *hub.Hub
	events *recorder
	log    *slog.Logger
	addr   string
	tlsCfg *tls.Config
	srv    *httptest.Server
	hubOpt hub.Options

	// ReleasesDir is where the API serves agent artifacts from, laid out as
	// <version>/<os>_<arch>/kuma-agent[.exe] like data_dir/releases.
	ReleasesDir string
	cookie      *http.Cookie // operator session, created on first use

	sseBroker  *sse.Broker
	registry   *registry.Registry
	sessions   *auth.Sessions
	limiter    *auth.Limiter
	termBroker *terminal.Broker
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWithTerminal(t, terminal.Options{})
}

// newHarnessWithTerminal is newHarness with custom terminal broker timings,
// for tests that need short ticket/pairing timeouts.
func newHarnessWithTerminal(t *testing.T, termOpts terminal.Options) *harness {
	t.Helper()
	dir := t.TempDir()
	if err := pki.Ensure(filepath.Join(dir, "pki"), []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := pki.TLSConfig(filepath.Join(dir, "pki"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "kb.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	h := &harness{
		t: t, dir: dir, st: st, events: &recorder{}, addr: addr, tlsCfg: tlsCfg,
		log:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		ReleasesDir: filepath.Join(dir, "releases"),
	}
	h.hubOpt = hub.Options{
		Store: st, Events: h.events, Log: h.log,
		MetricsInterval: 200 * time.Millisecond, HeartbeatInterval: 1 * time.Second, PongTimeout: 500 * time.Millisecond,
	}
	h.sseBroker = sse.New()
	h.registry = registry.New(st, h.sseBroker, 200*time.Millisecond)
	h.sessions = auth.NewSessions(st)
	h.limiter = auth.NewLimiter(5, time.Minute)
	h.termBroker = terminal.NewBrokerWithOptions(st, h.log, termOpts)
	t.Cleanup(h.termBroker.Close)
	h.hubOpt.TerminalRefused = func(dev, sid string) { h.termBroker.RefuseSession(dev, sid) }
	h.startServer()
	return h
}

func (h *harness) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /ws", h.hub)
	mux.Handle("GET /ws/terminal", h.termBroker)
	apiHandler := api.New(api.Deps{
		Store: h.st, Registry: h.registry, Hub: h.hub, Broker: h.sseBroker,
		Sessions: h.sessions, Limiter: h.limiter, Terminal: h.termBroker,
		AllowedHosts: map[string]bool{}, Log: h.log,
		ReleasesDir: h.ReleasesDir,
	})
	mux.Handle("/api/", apiHandler)
	return mux
}

// loginCookie creates an operator account and an authenticated session,
// returning the session cookie for use in HTTP requests against the harness.
func (h *harness) loginCookie() *http.Cookie {
	h.t.Helper()
	ctx := context.Background()
	hash, err := auth.HashPassword("pass")
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.UpsertUser(ctx, "admin", hash); err != nil {
		h.t.Fatal(err)
	}
	userID, _, err := h.st.GetUser(ctx, "admin")
	if err != nil {
		h.t.Fatal(err)
	}
	sid, exp, err := h.sessions.Create(ctx, userID)
	if err != nil {
		h.t.Fatal(err)
	}
	return &http.Cookie{Name: auth.CookieName, Value: sid, Expires: exp}
}

// session returns one operator session cookie for the whole test.
func (h *harness) session() *http.Cookie {
	h.t.Helper()
	if h.cookie == nil {
		h.cookie = h.loginCookie()
	}
	return h.cookie
}

// useTestReleaseKey makes agents built after this call trust a fresh release
// key, the way RELEASE_KEY_CURRENT does at build time, and returns the
// private half for signing. The compiled-in value is restored at cleanup.
func (h *harness) useTestReleaseKey() ed25519.PrivateKey {
	h.t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		h.t.Fatal(err)
	}
	prev := buildinfo.ReleaseKeyCurrentHex
	buildinfo.ReleaseKeyCurrentHex = hex.EncodeToString(pub)
	h.t.Cleanup(func() { buildinfo.ReleaseKeyCurrentHex = prev })
	return priv
}

// publishRelease does what "kumaboard sign" and "kumaboard release ingest"
// do for one artifact: it puts the artifact under ReleasesDir where the
// download endpoint looks for it, signs it with priv and ingests the release.
func (h *harness) publishRelease(version, goos, goarch string, artifact []byte, priv ed25519.PrivateKey) {
	h.t.Helper()
	name := "kuma-agent"
	if goos == "windows" {
		name = "kuma-agent.exe"
	}
	dir := filepath.Join(h.ReleasesDir, version, goos+"_"+goarch)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), artifact, 0o755); err != nil {
		h.t.Fatal(err)
	}
	sum := sha256.Sum256(artifact)
	hash := hex.EncodeToString(sum[:])
	sig := base64.StdEncoding.EncodeToString(signing.Sign(priv, signing.BuildMessage(version, goos, goarch, hash)))
	if err := h.st.IngestRelease(context.Background(), version, goos, goarch, hash, sig, int64(len(artifact))); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) startServer() {
	h.t.Helper()
	h.hub = hub.New(h.hubOpt)
	ln, err := net.Listen("tcp", h.addr)
	if err != nil {
		h.t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h.handler())
	srv.Listener = ln
	srv.TLS = h.tlsCfg
	srv.StartTLS()
	h.srv = srv
	h.t.Cleanup(srv.Close)
}

func (h *harness) stopServer() {
	h.srv.CloseClientConnections()
	h.srv.Close()
}

func (h *harness) agentConfig(name, token string) *config.Config {
	cfg := &config.Config{}
	cfg.Device.Name = name
	cfg.Server.URL = "wss://" + h.addr + "/ws"
	cfg.Token = token
	cfg.Capabilities = []string{proto.CapMetrics, proto.CapCustomCommands}
	cfg.Commands = map[string]config.Command{}
	caPEM, _ := os.ReadFile(filepath.Join(h.dir, "pki", "ca.pem"))
	cfg.CAPool = poolFrom(caPEM)
	return cfg
}

// registerDevice creates a device and returns its token.
func (h *harness) registerDevice(name string) string {
	h.t.Helper()
	_, token, err := h.st.CreateDevice(context.Background(), name, "", false, store.Schedule{})
	if err != nil {
		h.t.Fatal(err)
	}
	return token
}

type agentHandle struct {
	app    *app.App
	client *transport.Client
	cancel context.CancelFunc
}

// startAgent runs a real agent in the background. tweak may adjust the client.
func (h *harness) startAgent(cfg *config.Config, tweak func(*transport.Client)) *agentHandle {
	h.t.Helper()
	return h.startApp(app.New(cfg, h.log, upgrade.StartupResult{}), cfg, tweak)
}

// startApp runs an already-built agent app, for tests that adjust it first.
func (h *harness) startApp(a *app.App, cfg *config.Config, tweak func(*transport.Client)) *agentHandle {
	h.t.Helper()
	c := &transport.Client{
		URL:            cfg.Server.URL,
		TLS:            &tls.Config{RootCAs: cfg.CAPool, MinVersion: tls.VersionTLS12},
		Hello:          a.Hello,
		Handler:        a,
		Log:            h.log,
		SilenceTimeout: 500 * time.Millisecond,
		Backoff:        transport.NewBackoff(50*time.Millisecond, 200*time.Millisecond, 0.2),
	}
	if tweak != nil {
		tweak(c)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	h.t.Cleanup(cancel)
	return &agentHandle{app: a, client: c, cancel: cancel}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
