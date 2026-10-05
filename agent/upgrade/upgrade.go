package upgrade

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jhyoong/KumaBoard/internal/buildinfo"
	"github.com/jhyoong/KumaBoard/internal/signing"
	"github.com/jhyoong/KumaBoard/proto"
)

type Sender func(proto.UpgradeResult) error

type Upgrader struct {
	BinaryPath string
	BinaryDir  string
	OS         string
	Arch       string
	Keys       []ed25519.PublicKey
	DeviceName string
	Token      string
	HTTPClient *http.Client
	Log        *slog.Logger
	BaseURL    string
	// ConfigPath is forwarded to the selftest subprocess as -config so the
	// new binary validates the same config the running agent uses.
	ConfigPath string

	mu sync.Mutex
}

func New(binaryPath string, keys []ed25519.PublicKey, deviceName, token string, client *http.Client, log *slog.Logger) *Upgrader {
	return &Upgrader{
		BinaryPath: binaryPath,
		BinaryDir:  filepath.Dir(binaryPath),
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		Keys:       keys,
		DeviceName: deviceName,
		Token:      token,
		HTTPClient: client,
		Log:        log,
	}
}

func (u *Upgrader) HandleUpgradeRequest(req proto.UpgradeRequest, send Sender) {
	if !u.mu.TryLock() {
		u.Log.Warn("upgrade already in progress, ignoring request")
		return
	}
	defer u.mu.Unlock()

	from := buildinfo.Version
	report := func(state, reason string) {
		send(proto.UpgradeResult{FromVersion: from, ToVersion: req.Version, State: state, Reason: reason})
	}

	report(proto.UpgradeDownloading, "")
	tmpPath, err := u.download(req.URL)
	if err != nil {
		u.Log.Error("upgrade download failed", "err", err)
		report(proto.UpgradeFailed, proto.UpgradeReasonDownloadFailed)
		return
	}

	cleanup := func() { os.Remove(tmpPath) }

	report(proto.UpgradeVerifying, "")
	if err := u.verifyArtifact(tmpPath, req); err != nil {
		u.Log.Error("upgrade verify failed", "err", err)
		cleanup()
		report(proto.UpgradeFailed, proto.UpgradeReasonVerifyFailed)
		return
	}

	report(proto.UpgradeSelftest, "")
	if err := u.runSelftest(tmpPath); err != nil {
		attrs := []any{"err", err, "binary", tmpPath}
		var se *SelftestError
		if errors.As(err, &se) {
			attrs = append(attrs, "exit_code", se.ExitCode, "stderr_tail", se.Stderr, "stdout_tail", se.Stdout)
		}
		u.Log.Error("upgrade selftest failed", attrs...)
		cleanup()
		report(proto.UpgradeFailed, proto.UpgradeReasonSelftestFailed)
		return
	}

	if err := Swap(u.BinaryPath, tmpPath); err != nil {
		u.Log.Error("upgrade swap failed", "err", err)
		cleanup()
		report(proto.UpgradeFailed, proto.UpgradeReasonSwapFailed)
		return
	}
	report(proto.UpgradeSwapped, "")

	marker := filepath.Join(u.BinaryDir, PendingFile)
	WritePending(marker, &Pending{
		FromVersion: from,
		ToVersion:   req.Version,
		StartedAt:   time.Now().UTC(),
	})

	report(proto.UpgradeRestarting, "")
	u.Log.Info("upgrade: restarting", "from", from, "to", req.Version)
	if runtime.GOOS == "windows" {
		os.Exit(1)
	} else {
		os.Exit(0)
	}
}

func (u *Upgrader) download(url string) (string, error) {
	if !strings.HasPrefix(url, "http") {
		url = u.BaseURL + url
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+u.Token)
	req.Header.Set("X-Device-Name", u.DeviceName)

	client := u.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: status %d", resp.StatusCode)
	}
	tmp, err := os.CreateTemp(u.BinaryDir, tempPattern(runtime.GOOS))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	tmp.Close()
	os.Chmod(tmp.Name(), 0o755)
	return tmp.Name(), nil
}

func (u *Upgrader) verifyArtifact(path string, req proto.UpgradeRequest) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() != req.SizeBytes {
		return fmt.Errorf("verify: size %d, want %d", info.Size(), req.SizeBytes)
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != req.SHA256 {
		return fmt.Errorf("verify: sha256 %s, want %s", got, req.SHA256)
	}

	sig, err := base64.StdEncoding.DecodeString(req.Signature)
	if err != nil {
		return fmt.Errorf("verify: decode signature: %w", err)
	}
	msg := signing.BuildMessage(req.Version, u.OS, u.Arch, got)
	if !signing.VerifyAny(u.Keys, msg, sig) {
		return fmt.Errorf("verify: signature invalid")
	}
	return nil
}

// tempPattern is the os.CreateTemp pattern for the downloaded binary. On
// Windows the name must end in .exe: os/exec resolves an extensionless path
// through PATHEXT and reports "executable file not found" without ever
// starting the selftest child.
func tempPattern(goos string) string {
	if goos == "windows" {
		return "kuma-agent-upgrade-*.exe"
	}
	return "kuma-agent-upgrade-*"
}

// selftestArgs is the argv (after the binary path) of the selftest child.
func selftestArgs(configPath string) []string {
	args := []string{"selftest"}
	if configPath != "" {
		args = append(args, "-config", configPath)
	}
	return args
}

// SelftestError describes a failed selftest child. ExitCode is -1 when the
// child never started or was killed.
type SelftestError struct {
	Err      error
	ExitCode int
	Stdout   string
	Stderr   string
}

func (e *SelftestError) Error() string {
	if line := lastLine(e.Stderr); line != "" {
		return fmt.Sprintf("%v: %s", e.Err, line)
	}
	return e.Err.Error()
}

func (e *SelftestError) Unwrap() error { return e.Err }

// Selftest runs "<bin> selftest" exactly as the upgrade flow does.
func (u *Upgrader) Selftest(bin string) error {
	return u.runSelftest(bin)
}

func (u *Upgrader) runSelftest(binaryPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binaryPath, selftestArgs(u.ConfigPath)...)
	// The child's output is captured rather than wired to os.Stderr: under
	// the Windows service os.Stderr is not a usable handle, and a failed
	// write there would lose the output and fail an otherwise good run.
	var stdout, stderr tailBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	os.Stderr.Write(stdout.buf)
	os.Stderr.Write(stderr.buf)
	if err != nil {
		return &SelftestError{
			Err:      err,
			ExitCode: cmd.ProcessState.ExitCode(),
			Stdout:   strings.TrimSpace(string(stdout.buf)),
			Stderr:   strings.TrimSpace(string(stderr.buf)),
		}
	}
	return nil
}

// tailBuffer keeps the last few KiB written to it.
type tailBuffer struct {
	buf []byte
}

const tailMax = 4096

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailMax {
		t.buf = t.buf[len(t.buf)-tailMax:]
	}
	return len(p), nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
