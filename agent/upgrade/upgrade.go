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
	"sync/atomic"
	"time"

	"github.com/jhyoong/KumaBoard/internal/buildinfo"
	"github.com/jhyoong/KumaBoard/internal/signing"
	"github.com/jhyoong/KumaBoard/proto"
)

type Sender func(proto.UpgradeResult) error

// SelftestConfigStage is how a selftest child prefixes a config it rejects
// ("error: selftest: config: ..."). The running agent matches it in the
// child's stderr, so it must stay what released binaries print; a test in
// cmd/kuma-agent pins it.
const SelftestConfigStage = "selftest: config:"

const (
	downloadTimeout = 10 * time.Minute
	selftestTimeout = 15 * time.Second
)

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
	// running is the target version of the upgrade holding mu.
	running atomic.Pointer[string]

	// Zero means the default. Shortened in tests.
	downloadTimeout time.Duration
	selftestTimeout time.Duration
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

// Detail builds result detail text from parts the way every upgrade result
// does: bearer token redacted, length bounded.
func (u *Upgrader) Detail(parts ...string) string {
	return buildDetail(u.Token, parts...)
}

func (u *Upgrader) HandleUpgradeRequest(req proto.UpgradeRequest, send Sender) {
	from := buildinfo.Version
	result := func(state, reason, detail string) proto.UpgradeResult {
		return proto.UpgradeResult{
			FromVersion: from, ToVersion: req.Version, State: state, Reason: reason,
			UpgradeID: req.UpgradeID, Detail: buildDetail(u.Token, detail),
		}
	}

	if !u.mu.TryLock() {
		detail := "upgrade already in progress"
		if v := u.running.Load(); v != nil {
			detail = fmt.Sprintf("upgrade to %s already in progress", *v)
		}
		u.Log.Warn("upgrade already in progress, refusing request", "to", req.Version)
		// Not kept in the outbox: its one slot belongs to the running upgrade.
		if err := send(result(proto.UpgradeFailed, proto.UpgradeReasonBusy, detail)); err != nil {
			u.Log.Warn("upgrade: result not sent", "state", proto.UpgradeFailed, "err", err)
		}
		return
	}
	defer u.mu.Unlock()
	u.running.Store(&req.Version)
	defer u.running.Store(nil)

	// A failure that cannot be sent goes to the outbox for the next connect.
	// Retrying here would hold the lock across a disconnect of unknown length.
	report := func(state, reason, detail string) {
		res := result(state, reason, detail)
		err := send(res)
		if err == nil {
			return
		}
		u.Log.Warn("upgrade: result not sent", "state", state, "err", err)
		if state != proto.UpgradeFailed {
			return
		}
		if err := WriteOutbox(u.BinaryDir, res); err != nil {
			u.Log.Error("upgrade: failed to keep result for the next connect", "err", err)
		}
	}

	report(proto.UpgradeDownloading, "", "")
	tmpPath, err := u.download(req.URL)
	if err != nil {
		u.Log.Error("upgrade download failed", "err", err)
		report(proto.UpgradeFailed, proto.UpgradeReasonDownloadFailed, err.Error())
		return
	}

	cleanup := func() { os.Remove(tmpPath) }

	report(proto.UpgradeVerifying, "", "")
	if err := u.verifyArtifact(tmpPath, req); err != nil {
		u.Log.Error("upgrade verify failed", "err", err)
		cleanup()
		report(proto.UpgradeFailed, proto.UpgradeReasonVerifyFailed, buildDetail(u.Token, err.Error(), u.trustedKeys()))
		return
	}

	report(proto.UpgradeSelftest, "", "")
	if err := u.runSelftest(tmpPath); err != nil {
		attrs := []any{"err", err, "binary", tmpPath}
		var se *SelftestError
		if errors.As(err, &se) {
			attrs = append(attrs, "exit_code", se.ExitCode, "stderr_tail", se.Stderr, "stdout_tail", se.Stdout)
		}
		u.Log.Error("upgrade selftest failed", attrs...)
		cleanup()
		reason, detail := selftestFailure(u.Token, req.Version, err)
		report(proto.UpgradeFailed, reason, detail)
		return
	}

	if err := Swap(u.BinaryPath, tmpPath); err != nil {
		u.Log.Error("upgrade swap failed", "err", err)
		cleanup()
		report(proto.UpgradeFailed, proto.UpgradeReasonSwapFailed, err.Error())
		return
	}

	// The marker is written after the swap, not before: a crash between a
	// marker and the swap would look like a rollback. Without a marker the
	// new binary would start with no probation, so the swap is undone.
	marker := filepath.Join(u.BinaryDir, PendingFile)
	if err := WritePending(marker, &Pending{
		FromVersion: from,
		ToVersion:   req.Version,
		StartedAt:   time.Now().UTC(),
		UpgradeID:   req.UpgradeID,
	}); err != nil {
		os.Remove(marker)
		detail := fmt.Sprintf("write %s: %v; swap undone", PendingFile, err)
		if rerr := RestoreOld(u.BinaryPath); rerr != nil {
			detail = fmt.Sprintf("write %s: %v; undo failed: %v", PendingFile, err, rerr)
		} else {
			CleanupFailed(u.BinaryPath)
		}
		u.Log.Error("upgrade: pending marker not written", "detail", detail)
		report(proto.UpgradeFailed, proto.UpgradeReasonSwapFailed, detail)
		return
	}
	report(proto.UpgradeSwapped, "", "")

	report(proto.UpgradeRestarting, "", "")
	u.Log.Info("upgrade: restarting", "from", from, "to", req.Version)
	if runtime.GOOS == "windows" {
		exit(1)
	} else {
		exit(0)
	}
}

// download fetches the artifact into a temp file beside the binary. It runs
// under a deadline so a stalled transfer frees the upgrade lock. Its error
// text is sent to the server as the failure detail.
func (u *Upgrader) download(rawURL string) (string, error) {
	timeout := u.downloadTimeout
	if timeout == 0 {
		timeout = downloadTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	path, err := u.fetch(ctx, rawURL)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("download timed out after %s", shortDuration(timeout))
	}
	return path, err
}

func (u *Upgrader) fetch(ctx context.Context, rawURL string) (string, error) {
	if !strings.HasPrefix(rawURL, "http") {
		rawURL = u.BaseURL + rawURL
	}
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
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
		return "", fmt.Errorf("GET %s: status %d", req.URL.Path, resp.StatusCode)
	}
	tmp, err := os.CreateTemp(u.BinaryDir, tempPattern(runtime.GOOS))
	if err != nil {
		return "", err
	}
	_, err = io.Copy(tmp, resp.Body)
	if err == nil {
		// Flushed before it can be renamed over the running binary: the
		// rollback logic lives in this file, so it must not come up truncated.
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	os.Chmod(tmp.Name(), 0o755)
	return tmp.Name(), nil
}

// trustedKeys names the release keys compiled into this agent by the first
// 8 hex characters of each public key, for a verify failure's detail.
func (u *Upgrader) trustedKeys() string {
	s := fmt.Sprintf("agent trusts %d release key(s)", len(u.Keys))
	ids := make([]string, 0, len(u.Keys))
	for _, k := range u.Keys {
		h := hex.EncodeToString(k)
		ids = append(ids, h[:min(len(h), 8)])
	}
	if len(ids) > 0 {
		s += ": " + strings.Join(ids, ", ")
	}
	return s
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
	// Started is false when the child could not be executed at all.
	Started bool
	// TimedOut is set when the child was killed at the Timeout deadline.
	TimedOut bool
	Timeout  time.Duration
	Elapsed  time.Duration
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
	timeout := u.selftestTimeout
	if timeout == 0 {
		timeout = selftestTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binaryPath, selftestArgs(u.ConfigPath)...)
	// The child's output is captured rather than wired to os.Stderr: under
	// the Windows service os.Stderr is not a usable handle, and a failed
	// write there would lose the output and fail an otherwise good run.
	var stdout, stderr tailBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
	os.Stderr.Write(stdout.buf)
	os.Stderr.Write(stderr.buf)
	if err != nil {
		return &SelftestError{
			Err:      err,
			ExitCode: cmd.ProcessState.ExitCode(),
			Stdout:   strings.TrimSpace(string(stdout.buf)),
			Stderr:   strings.TrimSpace(string(stderr.buf)),
			Started:  cmd.ProcessState != nil,
			TimedOut: ctx.Err() != nil,
			Timeout:  timeout,
			Elapsed:  time.Since(start),
		}
	}
	return nil
}

// selftestFailure turns a runSelftest error into the reason and detail of
// the failed result. The detail is the outcome line followed by the child's
// output tails; a child that named the config stage gets its own reason.
func selftestFailure(token, version string, err error) (reason, detail string) {
	var se *SelftestError
	if !errors.As(err, &se) {
		return proto.UpgradeReasonSelftestFailed, buildDetail(token, fmt.Sprintf("selftest of %s: %v", version, err))
	}
	var outcome string
	switch {
	case se.TimedOut:
		outcome = "timed out after " + shortDuration(se.Timeout)
	case !se.Started:
		outcome = fmt.Sprintf("did not start: %v", se.Err)
	case se.ExitCode == -1:
		outcome = fmt.Sprintf("was killed after %.1fs: %v", se.Elapsed.Seconds(), se.Err)
	default:
		outcome = fmt.Sprintf("exited %d after %.1fs", se.ExitCode, se.Elapsed.Seconds())
	}
	reason = proto.UpgradeReasonSelftestFailed
	if strings.Contains(se.Stderr, SelftestConfigStage) {
		reason = proto.UpgradeReasonSelftestConfigRejected
	}
	return reason, buildDetail(token,
		fmt.Sprintf("selftest of %s %s", version, outcome),
		"--- stderr (last 4 KiB) ---", se.Stderr,
		"--- stdout (last 1 KiB) ---", tailBytes(se.Stdout, 1024))
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
