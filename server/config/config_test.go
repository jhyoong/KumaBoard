package config

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(write(t, "listen_addrs: [\"192.168.1.5:8443\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/var/lib/kumaboard" || cfg.MetricsIntervalS != 30 || cfg.HeartbeatIntervalS != 15 {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
}

func TestLoadRejectsWildcardBind(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8443", ":8443", "[::]:8443"} {
		if _, err := Load(write(t, "listen_addrs: [\""+addr+"\"]\n")); err == nil {
			t.Errorf("addr %q accepted", addr)
		}
	}
	if _, err := Load(write(t, "listen_addrs: []\n")); err == nil {
		t.Error("empty listen_addrs accepted")
	}
}

func TestHostsAndSANs(t *testing.T) {
	cfg, err := Load(write(t, "listen_addrs: [\"192.168.1.5:8443\", \"100.64.0.1:8443\"]\nhostnames: [\"kuma.local\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	sans := cfg.SANs()
	if len(sans) != 3 || sans[0] != "192.168.1.5" || sans[2] != "kuma.local" {
		t.Fatalf("SANs = %v", sans)
	}
	hosts := cfg.AllowedHosts()
	for _, h := range []string{"192.168.1.5:8443", "kuma.local", "kuma.local:8443"} {
		if !hosts[h] {
			t.Errorf("host %q not allowed: %v", h, hosts)
		}
	}
	if hosts["evil.example:8443"] {
		t.Error("unknown host allowed")
	}
}
