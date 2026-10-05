package upgrade

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// When KB_SELFTEST_HELPER is set, the test binary acts as a fake agent:
// it records its argv to KB_SELFTEST_ARGS and exits, optionally failing.
func TestMain(m *testing.M) {
	if os.Getenv("KB_SELFTEST_HELPER") == "1" {
		os.WriteFile(os.Getenv("KB_SELFTEST_ARGS"), []byte(strings.Join(os.Args[1:], "\n")), 0o600)
		if os.Getenv("KB_SELFTEST_FAIL") == "1" {
			fmt.Fprintln(os.Stderr, "starting")
			fmt.Fprintln(os.Stderr, "error: selftest: config: boom")
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runHelperSelftest(t *testing.T, configPath string) ([]string, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("KB_SELFTEST_HELPER", "1")
	t.Setenv("KB_SELFTEST_ARGS", argsFile)
	u := &Upgrader{ConfigPath: configPath}
	runErr := u.runSelftest(exe)
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(b), "\n"), runErr
}

func TestSelftestForwardsConfig(t *testing.T) {
	args, err := runHelperSelftest(t, "/opt/kuma/custom.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"selftest", "-config", "/opt/kuma/custom.yaml"}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("args = %q, want %q", args, want)
	}
}

func TestSelftestNoConfigPath(t *testing.T) {
	args, err := runHelperSelftest(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 1 || args[0] != "selftest" {
		t.Fatalf("args = %q, want [selftest]", args)
	}
}

func TestSelftestErrorIncludesStderr(t *testing.T) {
	t.Setenv("KB_SELFTEST_FAIL", "1")
	_, err := runHelperSelftest(t, "/x.yaml")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "error: selftest: config: boom") {
		t.Fatalf("error %q missing stderr line", err)
	}
	var se *SelftestError
	if !errors.As(err, &se) {
		t.Fatalf("error %T is not a *SelftestError", err)
	}
	if se.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", se.ExitCode)
	}
	if se.Stderr != "starting\nerror: selftest: config: boom" {
		t.Fatalf("stderr tail = %q", se.Stderr)
	}
}

func TestSelftestNotStarted(t *testing.T) {
	u := &Upgrader{}
	err := u.runSelftest(filepath.Join(t.TempDir(), "missing"))
	var se *SelftestError
	if !errors.As(err, &se) {
		t.Fatalf("error %v (%T) is not a *SelftestError", err, err)
	}
	if se.ExitCode != -1 {
		t.Fatalf("exit code = %d, want -1", se.ExitCode)
	}
}

func TestSelftestArgs(t *testing.T) {
	cfg := `C:\ProgramData\kuma-agent\config.yaml`
	got := selftestArgs(cfg)
	want := []string{"selftest", "-config", cfg}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("args = %q, want %q", got, want)
	}
	if got := selftestArgs(""); len(got) != 1 || got[0] != "selftest" {
		t.Fatalf("args = %q, want [selftest]", got)
	}
}

// The downloaded binary must be directly executable: on Windows os/exec
// refuses a path with no PATHEXT extension before spawning anything.
func TestTempPattern(t *testing.T) {
	for goos, wantExt := range map[string]string{"windows": ".exe", "linux": "", "darwin": ""} {
		p := tempPattern(goos)
		if !strings.HasPrefix(p, "kuma-agent-upgrade-*") {
			t.Fatalf("%s: pattern %q lost its prefix", goos, p)
		}
		f, err := os.CreateTemp(t.TempDir(), p)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		if ext := filepath.Ext(f.Name()); ext != wantExt {
			t.Fatalf("%s: temp name %q has ext %q, want %q", goos, f.Name(), ext, wantExt)
		}
	}
}
