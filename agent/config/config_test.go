package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jhyoong/KumaBoard/server/pki"
)

func setup(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	if err := pki.Ensure(dir, []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "token"), []byte("  secret-token\n"), 0o600)
	body := "server:\n  url: wss://127.0.0.1:8443/ws\n  ca_file: " + filepath.Join(dir, "ca.pem") +
		"\ntoken_file: " + filepath.Join(dir, "token") + "\n" + yaml
	p := filepath.Join(dir, "config.yaml")
	os.WriteFile(p, []byte(body), 0o600)
	return p
}

func TestLoadValid(t *testing.T) {
	cfg, err := Load(setup(t, `device:
  name: macos-desktop
capabilities: [metrics, custom-commands]
commands:
  uptime:
    description: "Uptime"
    run: ["/usr/bin/uptime"]
    timeout_s: 5
  sleep:
    run: ["/bin/true"]
    timeout_s: 15
    expect_disconnect: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Device.Name != "macos-desktop" || cfg.Token != "secret-token" || cfg.CAPool == nil {
		t.Fatalf("bad config: %+v", cfg)
	}
	defs := cfg.CommandDefs()
	if len(defs) != 2 || defs[0].Name != "sleep" || !defs[0].ExpectDisconnect || defs[1].TimeoutS != 5 {
		t.Fatalf("bad command defs: %+v", defs)
	}
}

func TestLoadSetsAbsolutePath(t *testing.T) {
	p := setup(t, "device:\n  name: x\n")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, p)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(rel)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Path != p || !filepath.IsAbs(cfg.Path) {
		t.Fatalf("Path = %q, want %q", cfg.Path, p)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]string{
		"missing name":   "capabilities: [metrics]\n",
		"empty run":      "device: {name: x}\ncommands:\n  a: {run: [], timeout_s: 5}\n",
		"zero timeout":   "device: {name: x}\ncommands:\n  a: {run: [/bin/true], timeout_s: 0}\n",
		"bad name chars": "device: {name: \"has space\"}\n",
	}
	for label, body := range cases {
		if _, err := Load(setup(t, body)); err == nil {
			t.Errorf("%s: accepted", label)
		}
	}
}

func TestLoadRejectsNonWSS(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	os.WriteFile(p, []byte("device: {name: x}\nserver: {url: ws://127.0.0.1/ws, ca_file: /nonexistent}\ntoken_file: /nonexistent\n"), 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("ws:// accepted")
	}
}
