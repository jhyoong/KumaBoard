package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/server/auth"
	"github.com/jhyoong/KumaBoard/server/hub"
	"github.com/jhyoong/KumaBoard/server/registry"
	"github.com/jhyoong/KumaBoard/server/sse"
	"github.com/jhyoong/KumaBoard/server/store"
	"github.com/jhyoong/KumaBoard/server/terminal"
)

type env struct {
	srv    *httptest.Server
	st     *store.Store
	term   *terminal.Broker
	reg    *registry.Registry
	client *http.Client
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWake(t, nil)
}

// newEnvWake is newEnv with a Wake-on-LAN sender wired in.
func newEnvWake(t *testing.T, wake WakeFunc) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hash, _ := auth.HashPassword("correct-horse-battery")
	st.UpsertUser(context.Background(), "admin", hash)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	broker := sse.New()
	reg := registry.New(st, broker, 30*time.Second)
	reg.Load(context.Background())
	h := hub.New(hub.Options{Store: st, Events: reg, Log: log, MetricsInterval: 30 * time.Second, HeartbeatInterval: 15 * time.Second})
	term := terminal.NewBroker(st, log)
	t.Cleanup(term.Close)
	handler := New(Deps{
		Store: st, Registry: reg, Hub: h, Broker: broker,
		Sessions: auth.NewSessions(st), Limiter: auth.NewLimiter(5, time.Minute), Terminal: term,
		AllowedHosts: map[string]bool{"127.0.0.1": true}, Log: log, Wake: wake,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	jar := &cookieJar{}
	return &env{srv: srv, st: st, term: term, reg: reg, client: &http.Client{Jar: jar}}
}

func (e *env) do(t *testing.T, method, path string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (e *env) login(t *testing.T) {
	t.Helper()
	resp := e.do(t, "POST", "/api/login", map[string]string{"username": "admin", "password": "correct-horse-battery"})
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
}

func TestUnauthenticatedIs401(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/api/devices", "/api/audit", "/api/events", "/api/me"} {
		if resp := e.do(t, "GET", p, nil); resp.StatusCode != 401 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 5; i++ {
		resp := e.do(t, "POST", "/api/login", map[string]string{"username": "admin", "password": "wrong"})
		if resp.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i, resp.StatusCode)
		}
	}
	resp := e.do(t, "POST", "/api/login", map[string]string{"username": "admin", "password": "correct-horse-battery"})
	if resp.StatusCode != 429 {
		t.Fatalf("expected lockout, got %d", resp.StatusCode)
	}
	entries, _ := e.st.ListAudit(context.Background(), 10, 0)
	if len(entries) < 5 || entries[0].Action != "login" {
		t.Fatalf("login failures not audited: %+v", entries)
	}
}

func TestDeviceRegisterAndList(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	resp := e.do(t, "POST", "/api/devices", map[string]any{"name": "macos-desktop", "mac": "aa:bb:cc:dd:ee:ff", "normally_off": false})
	if resp.StatusCode != 201 {
		t.Fatalf("register: %d", resp.StatusCode)
	}
	var reg struct {
		Token  string           `json:"token"`
		Device registry.Summary `json:"device"`
	}
	json.NewDecoder(resp.Body).Decode(&reg)
	if reg.Token == "" || reg.Device.Name != "macos-desktop" || reg.Device.State != registry.OfflineUnexpected {
		t.Fatalf("bad register response: %+v", reg)
	}
	resp = e.do(t, "GET", "/api/devices", nil)
	var list []registry.Summary
	json.NewDecoder(resp.Body).Decode(&list)
	if len(list) != 1 || list[0].Name != "macos-desktop" {
		t.Fatalf("list: %+v", list)
	}
	resp = e.do(t, "PATCH", "/api/devices/macos-desktop", map[string]any{"mac": "11:22:33:44:55:66", "normally_off": true,
		"schedule": map[string]any{"expected_offline": []any{}, "grace_period_s": 60}})
	if resp.StatusCode != 200 {
		t.Fatalf("patch: %d", resp.StatusCode)
	}
	resp = e.do(t, "POST", "/api/devices/macos-desktop/revoke", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	d, _ := e.st.GetDevice(context.Background(), "macos-desktop")
	if d.TokenHash != nil || !d.NormallyOff {
		t.Fatalf("device not updated: %+v", d)
	}
}

func TestLogout(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	if resp := e.do(t, "GET", "/api/me", nil); resp.StatusCode != 200 {
		t.Fatal("me after login")
	}
	e.do(t, "POST", "/api/logout", nil)
	if resp := e.do(t, "GET", "/api/me", nil); resp.StatusCode != 401 {
		t.Fatal("me after logout")
	}
}
