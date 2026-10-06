package upgrade

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
)

// When KB_SELFTEST_HELPER is set, the test binary acts as a fake agent:
// it records its argv to KB_SELFTEST_ARGS and exits, optionally failing at
// the stage named by KB_SELFTEST_STAGE (default config) or, with
// KB_SELFTEST_HANG, never exiting on its own.
func TestMain(m *testing.M) {
	if os.Getenv("KB_SELFTEST_HELPER") == "1" {
		os.WriteFile(os.Getenv("KB_SELFTEST_ARGS"), []byte(strings.Join(os.Args[1:], "\n")), 0o600)
		if os.Getenv("KB_SELFTEST_HANG") == "1" {
			time.Sleep(time.Minute)
		}
		if os.Getenv("KB_SELFTEST_FAIL") == "1" {
			stage := os.Getenv("KB_SELFTEST_STAGE")
			if stage == "" {
				stage = "config"
			}
			fmt.Println("checking")
			fmt.Fprintln(os.Stderr, "starting")
			fmt.Fprintf(os.Stderr, "error: selftest: %s: boom\n", stage)
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

func TestSelftestDetailExit(t *testing.T) {
	t.Setenv("KB_SELFTEST_FAIL", "1")
	_, err := runHelperSelftest(t, "/x.yaml")
	reason, detail := selftestFailure("", "0.2.5", err)
	if reason != proto.UpgradeReasonSelftestConfigRejected {
		t.Fatalf("reason = %q, want selftest_config_rejected", reason)
	}
	want := regexp.MustCompile(`^selftest of 0\.2\.5 exited 1 after \d+\.\ds
--- stderr \(last 4 KiB\) ---
starting
error: selftest: config: boom
--- stdout \(last 1 KiB\) ---
checking$`)
	if !want.MatchString(detail) {
		t.Fatalf("detail:\n%s", detail)
	}
}

func TestSelftestOtherStagesAreNotConfigRejected(t *testing.T) {
	t.Setenv("KB_SELFTEST_FAIL", "1")
	for _, stage := range []string{"collectors", "pty"} {
		t.Setenv("KB_SELFTEST_STAGE", stage)
		_, err := runHelperSelftest(t, "/x.yaml")
		reason, detail := selftestFailure("", "0.2.5", err)
		if reason != proto.UpgradeReasonSelftestFailed {
			t.Errorf("%s: reason = %q, want selftest_failed", stage, reason)
		}
		if !strings.Contains(detail, "error: selftest: "+stage+": boom") {
			t.Errorf("%s: detail missing the stderr line:\n%s", stage, detail)
		}
	}
}

func TestSelftestDetailNotStarted(t *testing.T) {
	u := &Upgrader{}
	err := u.runSelftest(filepath.Join(t.TempDir(), "missing"))
	reason, detail := selftestFailure("", "0.2.5", err)
	if reason != proto.UpgradeReasonSelftestFailed {
		t.Fatalf("reason = %q", reason)
	}
	// No output, so the first line is followed by the two empty sections.
	first, rest, _ := strings.Cut(detail, "\n")
	if !strings.HasPrefix(first, "selftest of 0.2.5 did not start: ") || len(first) == len("selftest of 0.2.5 did not start: ") {
		t.Fatalf("first line = %q", first)
	}
	if rest != "--- stderr (last 4 KiB) ---\n--- stdout (last 1 KiB) ---" {
		t.Fatalf("rest = %q", rest)
	}
}

func TestSelftestDetailTimeout(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KB_SELFTEST_HELPER", "1")
	t.Setenv("KB_SELFTEST_HANG", "1")
	t.Setenv("KB_SELFTEST_ARGS", filepath.Join(t.TempDir(), "args"))
	u := &Upgrader{selftestTimeout: 300 * time.Millisecond}
	start := time.Now()
	runErr := u.runSelftest(exe)
	if time.Since(start) > 20*time.Second {
		t.Fatalf("selftest ran %v, the deadline did not kill it", time.Since(start))
	}
	var se *SelftestError
	if !errors.As(runErr, &se) || !se.TimedOut {
		t.Fatalf("error %v (%T), want a timed-out *SelftestError", runErr, runErr)
	}
	reason, detail := selftestFailure("", "0.2.5", runErr)
	if reason != proto.UpgradeReasonSelftestFailed {
		t.Fatalf("reason = %q", reason)
	}
	if first, _, _ := strings.Cut(detail, "\n"); first != "selftest of 0.2.5 timed out after 300ms" {
		t.Fatalf("first line = %q", first)
	}
	// The production deadline reads the way the design quotes it.
	se.Timeout = selftestTimeout
	if _, detail := selftestFailure("", "0.2.5", se); !strings.HasPrefix(detail, "selftest of 0.2.5 timed out after 15s\n") {
		t.Fatalf("detail = %q", detail)
	}
}

func TestSelftestDetailRedactsAndTrimsStdout(t *testing.T) {
	se := &SelftestError{
		Err: errors.New("exit status 1"), ExitCode: 1, Started: true, Elapsed: 400 * time.Millisecond,
		Stderr: "error: selftest: pty: token secret-token leaked",
		Stdout: strings.Repeat("a", 3000) + "END",
	}
	_, detail := selftestFailure("secret-token", "0.2.5", se)
	if strings.Contains(detail, "secret-token") || !strings.Contains(detail, "token [redacted] leaked") {
		t.Fatalf("token not redacted:\n%s", detail)
	}
	if !strings.HasPrefix(detail, "selftest of 0.2.5 exited 1 after 0.4s\n") {
		t.Fatalf("first line: %q", detail[:60])
	}
	_, stdout, _ := strings.Cut(detail, "--- stdout (last 1 KiB) ---\n")
	if len(stdout) != 1024 || !strings.HasSuffix(stdout, "END") {
		t.Fatalf("stdout section is %d bytes", len(stdout))
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
