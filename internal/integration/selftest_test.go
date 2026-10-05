package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jhyoong/KumaBoard/agent/config"
	"github.com/jhyoong/KumaBoard/agent/upgrade"
	"github.com/jhyoong/KumaBoard/server/pki"
)

// TestSelftestSubprocessWithCustomConfig builds the real kuma-agent binary and
// runs the pre-upgrade selftest against a config at a non-default path, the
// way an agent started with "run -config PATH" would.
func TestSelftestSubprocessWithCustomConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("builds kuma-agent; skipped under -short")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "kuma-agent")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command(goBin, "build", "-o", bin, "./cmd/kuma-agent")
	build.Dir = filepath.Join("..", "..")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build kuma-agent: %v\n%s", err, out)
	}

	pkiDir := filepath.Join(dir, "pki")
	if err := pki.Ensure(pkiDir, []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "custom", "agent.yaml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	// With terminal enabled the selftest spawns a real PTY through the
	// production spawn path (Windows reports unsupported and passes).
	caps := "[metrics]"
	if f, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0); err == nil || runtime.GOOS == "windows" {
		if f != nil {
			f.Close()
		}
		caps = "[metrics, terminal]"
	}
	body := "device:\n  name: selftest-dev\nserver:\n  url: wss://127.0.0.1:8443/ws\n  ca_file: " +
		filepath.Join(pkiDir, "ca.pem") + "\ntoken_file: " + tokenPath + "\ncapabilities: " + caps + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	// Make sure the child's default config lookup cannot find a real config.
	t.Setenv("KUMA_AGENT_CONFIG", filepath.Join(dir, "does-not-exist.yaml"))

	u := &upgrade.Upgrader{ConfigPath: cfg.Path}
	if err := u.Selftest(bin); err != nil {
		t.Fatalf("selftest with -config: %v", err)
	}

	u.ConfigPath = ""
	if err := u.Selftest(bin); err == nil {
		t.Fatal("selftest without -config succeeded; expected default config lookup to fail")
	}
}
