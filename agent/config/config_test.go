package config

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
		"missing name":    "capabilities: [metrics]\n",
		"empty run":       "device: {name: x}\ncommands:\n  a: {run: [], timeout_s: 5}\n",
		"zero timeout":    "device: {name: x}\ncommands:\n  a: {run: [/bin/true], timeout_s: 0}\n",
		"bad name chars":  "device: {name: \"has space\"}\n",
		"no run/script":   "device: {name: x}\ncommands:\n  a: {timeout_s: 5}\n",
		"empty argv[0]":   "device: {name: x}\ncommands:\n  a: {run: [\"\"], script: /opt/s.sh, timeout_s: 5}\n",
		"relative script": "device: {name: x}\ncommands:\n  a: {script: s.sh, timeout_s: 5}\n",
		"script timeout":  "device: {name: x}\ncommands:\n  a: {script: /opt/s.sh}\n",
		"bad cmd name":    "device: {name: x}\ncommands:\n  \"Bad Name\": {script: /opt/s.sh, timeout_s: 5}\n",
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

func TestScriptAndConfirm(t *testing.T) {
	cfg, err := Load(setup(t, `device:
  name: x
commands:
  direct:
    description: "Run the script itself"
    script: /opt/kuma-scripts/backup.sh
    timeout_s: 60
    confirm: true
  via:
    run: ["/bin/bash", "-eu"]
    script: /opt/kuma-scripts/clean.sh
    timeout_s: 30
  plain:
    run: ["/usr/bin/uptime"]
    timeout_s: 5
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Commands["direct"].Argv(); !slices.Equal(got, []string{"/opt/kuma-scripts/backup.sh"}) {
		t.Fatalf("direct argv: %v", got)
	}
	if got := cfg.Commands["via"].Argv(); !slices.Equal(got, []string{"/bin/bash", "-eu", "/opt/kuma-scripts/clean.sh"}) {
		t.Fatalf("via argv: %v", got)
	}
	if got := cfg.Commands["plain"].Argv(); !slices.Equal(got, []string{"/usr/bin/uptime"}) {
		t.Fatalf("plain argv: %v", got)
	}
	defs := cfg.CommandDefs()
	if len(defs) != 3 || defs[0].Name != "direct" || !defs[0].Confirm || defs[1].Confirm || defs[2].Confirm {
		t.Fatalf("defs: %+v", defs)
	}
	// Argv must not alias Run: a caller appending to it cannot change the config.
	c := cfg.Commands["via"]
	_ = append(c.Argv(), "extra")
	if len(c.Run) != 2 {
		t.Fatalf("Run changed: %v", c.Run)
	}
}

func TestLoadCommands(t *testing.T) {
	// Only commands: is read. The rest of the file may be anything, including
	// values Load would reject or files that do not exist.
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("device: {name: \"NOT VALID\"}\nserver: {url: http://x, ca_file: /nonexistent}\n" +
		"commands:\n  a: {script: /opt/a.sh, timeout_s: 5, confirm: true}\n")
	cmds, err := LoadCommands(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 || cmds["a"].Script != "/opt/a.sh" || !cmds["a"].Confirm {
		t.Fatalf("cmds: %+v", cmds)
	}

	write("device: {name: x}\n")
	if cmds, err = LoadCommands(p); err != nil || cmds == nil || len(cmds) != 0 {
		t.Fatalf("no commands block: %v %v", cmds, err)
	}

	for label, body := range map[string]string{
		"invalid yaml":  "commands:\n  a: {run: [/bin/true\n",
		"invalid entry": "commands:\n  a: {run: [/bin/true], timeout_s: 0}\n",
		"wrong type":    "commands: [1, 2]\n",
	} {
		write(body)
		if _, err := LoadCommands(p); err == nil {
			t.Errorf("%s: accepted", label)
		}
	}
	if _, err := LoadCommands(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("missing file accepted")
	}
}

func TestRestartOnlyChanges(t *testing.T) {
	p := setup(t, "device:\n  name: x\ncapabilities: [metrics]\ncommands:\n  a: {run: [/bin/true], timeout_s: 5}\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	same, _ := os.ReadFile(p)
	if got := cfg.RestartOnlyChanges(same); len(got) != 0 {
		t.Fatalf("unchanged file reported %v", got)
	}
	// A commands-only edit is live, so it is not reported.
	edited := append(append([]byte(nil), same...), []byte("  b: {run: [/bin/true], timeout_s: 5}\n")...)
	if got := cfg.RestartOnlyChanges(edited); len(got) != 0 {
		t.Fatalf("commands edit reported %v", got)
	}
	other := []byte("device: {name: y}\nserver: {url: wss://other/ws}\ntoken_file: /elsewhere\ncapabilities: [metrics, terminal]\n")
	if got := cfg.RestartOnlyChanges(other); !slices.Equal(got, []string{"device", "server", "token_file", "capabilities"}) {
		t.Fatalf("got %v", got)
	}
	if got := cfg.RestartOnlyChanges([]byte("commands: [")); got != nil {
		t.Fatalf("unparseable content reported %v", got)
	}
}

// The shipped example configs must keep parsing: their commands: blocks are
// what operators copy.
func TestExampleConfigCommandsParse(t *testing.T) {
	examples := map[string]int{"linux": 1, "macos": 2, "pi": 0}
	if runtime.GOOS == "windows" {
		// Script paths must be absolute for the OS the agent runs on.
		examples = map[string]int{"windows": 1}
	}
	for name, want := range examples {
		cmds, err := LoadCommands(filepath.Join("..", "..", "configs", "kuma-agent."+name+".example.yaml"))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if len(cmds) != want {
			t.Errorf("%s: %d commands, want %d", name, len(cmds), want)
		}
	}
}
