package upgrade

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/internal/buildinfo"
	"github.com/jhyoong/KumaBoard/internal/signing"
	"github.com/jhyoong/KumaBoard/proto"
)

func testKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestVerifyArtifact(t *testing.T) {
	pub, priv := testKeys(t)
	data := []byte("fake-binary-content")
	h := sha256.Sum256(data)
	sha := hex.EncodeToString(h[:])
	msg := signing.BuildMessage("0.4.0", runtime.GOOS, runtime.GOARCH, sha)
	sig := base64.StdEncoding.EncodeToString(signing.Sign(priv, msg))

	u := &Upgrader{
		BinaryDir: t.TempDir(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Keys:      []ed25519.PublicKey{pub},
	}

	tmp := filepath.Join(u.BinaryDir, "kuma-agent.new")
	os.WriteFile(tmp, data, 0o755)

	req := proto.UpgradeRequest{
		Version: "0.4.0", OS: runtime.GOOS, Arch: runtime.GOARCH,
		SHA256: sha, SizeBytes: int64(len(data)), Signature: sig,
	}
	if err := u.verifyArtifact(tmp, req); err != nil {
		t.Fatalf("valid artifact rejected: %v", err)
	}

	badReq := req
	badReq.SizeBytes = 999
	if err := u.verifyArtifact(tmp, badReq); err == nil {
		t.Fatal("wrong size accepted")
	}

	badReq = req
	badReq.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := u.verifyArtifact(tmp, badReq); err == nil {
		t.Fatal("wrong hash accepted")
	}

	badReq = req
	badReq.Signature = base64.StdEncoding.EncodeToString([]byte("bad-sig-that-is-not-64-bytes-but-will-fail-verify"))
	if err := u.verifyArtifact(tmp, badReq); err == nil {
		t.Fatal("wrong signature accepted")
	}

	badReq = req
	badReq.Version = "0.5.0"
	if err := u.verifyArtifact(tmp, badReq); err == nil {
		t.Fatal("version mismatch accepted")
	}
}

func TestDownload(t *testing.T) {
	data := []byte("binary-content-for-download-test")
	var gotAuth, gotDevice string
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotDevice = r.Header.Get("X-Device-Name")
		w.Write(data)
	}))
	defer ts.Close()

	u := &Upgrader{
		BinaryDir:  t.TempDir(),
		DeviceName: "test-device",
		Token:      "secret-token",
		HTTPClient: ts.Client(),
	}

	path, err := u.download(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	got, _ := os.ReadFile(path)
	if string(got) != string(data) {
		t.Fatalf("downloaded content mismatch")
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotDevice != "test-device" {
		t.Fatalf("device = %q", gotDevice)
	}
}

const testUpgradeID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

// flow drives HandleUpgradeRequest end to end against a local TLS server.
// The artifact is this test binary, which acts as the selftest child (see
// TestMain); the "installed" binary is a scratch file.
type flow struct {
	u   *Upgrader
	req proto.UpgradeRequest
	pub ed25519.PublicKey

	mu      sync.Mutex
	results []proto.UpgradeResult
	sendErr error
}

func newFlow(t *testing.T) *flow {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/releases/artifact" {
			http.NotFound(w, r)
			return
		}
		w.Write(artifact)
	}))
	t.Cleanup(ts.Close)

	pub, priv := testKeys(t)
	h := sha256.Sum256(artifact)
	sha := hex.EncodeToString(h[:])
	sig := signing.Sign(priv, signing.BuildMessage("0.4.0", runtime.GOOS, runtime.GOARCH, sha))

	dir := t.TempDir()
	bin := filepath.Join(dir, agentBinaryName(runtime.GOOS))
	os.WriteFile(bin, []byte("current-binary"), 0o755)
	u := New(bin, []ed25519.PublicKey{pub}, "test-device", "secret-token", ts.Client(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	u.BaseURL = ts.URL
	u.ConfigPath = "/x.yaml"

	t.Setenv("KB_SELFTEST_HELPER", "1")
	t.Setenv("KB_SELFTEST_ARGS", filepath.Join(t.TempDir(), "args"))
	return &flow{u: u, pub: pub, req: proto.UpgradeRequest{
		Version: "0.4.0", OS: runtime.GOOS, Arch: runtime.GOARCH,
		SHA256: sha, SizeBytes: int64(len(artifact)),
		Signature: base64.StdEncoding.EncodeToString(sig),
		URL:       "/api/agent/releases/artifact", UpgradeID: testUpgradeID,
	}}
}

func (f *flow) send(res proto.UpgradeResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results = append(f.results, res)
	return f.sendErr
}

// run handles the request and checks what every result has in common.
func (f *flow) run(t *testing.T) []proto.UpgradeResult {
	t.Helper()
	f.u.HandleUpgradeRequest(f.req, f.send)
	for _, r := range f.results {
		if r.UpgradeID != testUpgradeID || r.ToVersion != "0.4.0" || r.FromVersion != buildinfo.Version {
			t.Fatalf("result %+v does not echo the request", r)
		}
		if len(r.Detail) > proto.MaxUpgradeDetailLen || strings.Contains(r.Detail, "secret-token") {
			t.Fatalf("detail unbounded or leaking the token: %q", r.Detail)
		}
		if r.State != proto.UpgradeFailed && (r.Reason != "" || r.Detail != "") {
			t.Fatalf("progress result carries a reason or detail: %+v", r)
		}
	}
	return f.results
}

func states(results []proto.UpgradeResult) string {
	var s []string
	for _, r := range results {
		s = append(s, r.State)
	}
	return strings.Join(s, " ")
}

// wantFailed checks the flow reported exactly these states, ending in a
// failure with this reason, and left the installed binary alone.
func (f *flow) wantFailed(t *testing.T, wantStates, reason string) proto.UpgradeResult {
	t.Helper()
	results := f.run(t)
	if got := states(results); got != wantStates {
		t.Fatalf("states = %q, want %q", got, wantStates)
	}
	last := results[len(results)-1]
	if last.Reason != reason {
		t.Fatalf("reason = %q, want %q (detail %q)", last.Reason, reason, last.Detail)
	}
	if got, _ := os.ReadFile(f.u.BinaryPath); string(got) != "current-binary" {
		t.Fatalf("installed binary changed: %q", got)
	}
	if left, _ := filepath.Glob(filepath.Join(f.u.BinaryDir, "kuma-agent-upgrade-*")); len(left) != 0 {
		t.Fatalf("download left behind: %v", left)
	}
	return last
}

func TestDownloadStatusDetail(t *testing.T) {
	f := newFlow(t)
	f.req.URL = "/api/agent/releases/gone?x=1"
	last := f.wantFailed(t, "downloading failed", proto.UpgradeReasonDownloadFailed)
	if last.Detail != "GET /api/agent/releases/gone: status 404" {
		t.Fatalf("detail = %q", last.Detail)
	}
}

func TestDownloadTransportErrorDetail(t *testing.T) {
	f := newFlow(t)
	f.u.BaseURL = "https://127.0.0.1:1"
	last := f.wantFailed(t, "downloading failed", proto.UpgradeReasonDownloadFailed)
	if !strings.Contains(last.Detail, "127.0.0.1:1") {
		t.Fatalf("detail = %q, want the transport error", last.Detail)
	}
}

func TestDownloadTimeoutDetail(t *testing.T) {
	stalled := make(chan struct{})
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Headers and a first chunk arrive, then the transfer stalls.
		w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		select {
		case <-stalled:
		case <-r.Context().Done():
		}
	}))
	defer ts.Close()
	defer close(stalled)

	f := newFlow(t)
	f.u.HTTPClient = ts.Client()
	f.u.BaseURL = ts.URL
	f.u.downloadTimeout = 200 * time.Millisecond
	start := time.Now()
	last := f.wantFailed(t, "downloading failed", proto.UpgradeReasonDownloadFailed)
	if last.Detail != "download timed out after 200ms" {
		t.Fatalf("detail = %q", last.Detail)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("stalled download held the upgrade for %v", time.Since(start))
	}
	// The lock is free again.
	if !f.u.mu.TryLock() {
		t.Fatal("upgrade lock still held after the timeout")
	}
	f.u.mu.Unlock()
	// The production deadline reads the way the design quotes it.
	if got := "download timed out after " + shortDuration(downloadTimeout); got != "download timed out after 10m" {
		t.Fatalf("production text = %q", got)
	}
}

func TestVerifyDetailNamesTrustedKeys(t *testing.T) {
	f := newFlow(t)
	f.req.SizeBytes++
	last := f.wantFailed(t, "downloading verifying failed", proto.UpgradeReasonVerifyFailed)
	want := fmt.Sprintf("verify: size %d, want %d\nagent trusts 1 release key(s): %s",
		f.req.SizeBytes-1, f.req.SizeBytes, hex.EncodeToString(f.pub)[:8])
	if last.Detail != want {
		t.Fatalf("detail = %q, want %q", last.Detail, want)
	}

	// Signed by a key this agent does not trust, with two keys configured.
	f = newFlow(t)
	other, _ := testKeys(t)
	f.u.Keys = []ed25519.PublicKey{other, other}
	last = f.wantFailed(t, "downloading verifying failed", proto.UpgradeReasonVerifyFailed)
	id := hex.EncodeToString(other)[:8]
	if last.Detail != "verify: signature invalid\nagent trusts 2 release key(s): "+id+", "+id {
		t.Fatalf("detail = %q", last.Detail)
	}

	f = newFlow(t)
	f.u.Keys = nil
	last = f.wantFailed(t, "downloading verifying failed", proto.UpgradeReasonVerifyFailed)
	if last.Detail != "verify: signature invalid\nagent trusts 0 release key(s)" {
		t.Fatalf("detail = %q", last.Detail)
	}
}

func TestSelftestConfigRejectedFlow(t *testing.T) {
	t.Setenv("KB_SELFTEST_FAIL", "1")
	f := newFlow(t)
	last := f.wantFailed(t, "downloading verifying selftest failed", proto.UpgradeReasonSelftestConfigRejected)
	if !strings.HasPrefix(last.Detail, "selftest of 0.4.0 exited 1 after ") ||
		!strings.Contains(last.Detail, "\n--- stderr (last 4 KiB) ---\nstarting\nerror: selftest: config: boom\n--- stdout (last 1 KiB) ---") {
		t.Fatalf("detail:\n%s", last.Detail)
	}
}

func TestSelftestOtherStageFlow(t *testing.T) {
	t.Setenv("KB_SELFTEST_FAIL", "1")
	t.Setenv("KB_SELFTEST_STAGE", "collectors")
	f := newFlow(t)
	last := f.wantFailed(t, "downloading verifying selftest failed", proto.UpgradeReasonSelftestFailed)
	if !strings.Contains(last.Detail, "error: selftest: collectors: boom") {
		t.Fatalf("detail:\n%s", last.Detail)
	}
}

func TestBusyReply(t *testing.T) {
	f := newFlow(t)
	// Another upgrade, to 0.3.9, holds the lock.
	running := "0.3.9"
	f.u.mu.Lock()
	f.u.running.Store(&running)
	results := f.run(t)
	f.u.mu.Unlock()

	if len(results) != 1 {
		t.Fatalf("got %d results, want one busy reply: %+v", len(results), results)
	}
	r := results[0]
	if r.State != proto.UpgradeFailed || r.Reason != proto.UpgradeReasonBusy {
		t.Fatalf("result %+v, want failed/busy", r)
	}
	if r.Detail != "upgrade to 0.3.9 already in progress" {
		t.Fatalf("detail = %q", r.Detail)
	}
	if got, _ := os.ReadFile(f.u.BinaryPath); string(got) != "current-binary" {
		t.Fatalf("installed binary changed: %q", got)
	}
}

// A busy reply that cannot be sent is dropped: the outbox slot belongs to
// the upgrade that is running.
func TestBusyReplyNotKeptInOutbox(t *testing.T) {
	f := newFlow(t)
	f.sendErr = errors.New("socket down")
	f.u.mu.Lock()
	f.run(t)
	f.u.mu.Unlock()
	if res, err := ReadOutbox(f.u.BinaryDir); res != nil || err != nil {
		t.Fatalf("outbox = %+v, %v, want empty", res, err)
	}
}

func TestFailedResultKeptWhenSendFails(t *testing.T) {
	f := newFlow(t)
	f.sendErr = errors.New("socket down")
	f.req.URL = "/api/agent/releases/gone"
	last := f.wantFailed(t, "downloading failed", proto.UpgradeReasonDownloadFailed)

	kept, err := ReadOutbox(f.u.BinaryDir)
	if err != nil || kept == nil {
		t.Fatalf("outbox = %+v, %v, want the failed result", kept, err)
	}
	if *kept != last || kept.Detail != "GET /api/agent/releases/gone: status 404" {
		t.Fatalf("kept %+v, want %+v", kept, last)
	}

	// Next connect: delivered once, then gone.
	var delivered []proto.UpgradeResult
	sent, err := FlushOutbox(f.u.BinaryDir, func(r proto.UpgradeResult) error {
		delivered = append(delivered, r)
		return nil
	})
	if !sent || err != nil || len(delivered) != 1 || delivered[0] != last {
		t.Fatalf("flush = %v, %v, %+v", sent, err, delivered)
	}
	if res, _ := ReadOutbox(f.u.BinaryDir); res != nil {
		t.Fatal("outbox not emptied")
	}
}

// Progress reports are not worth keeping: only a terminal result is.
func TestProgressResultNotKeptWhenSendFails(t *testing.T) {
	code := stubExit(t)
	f := newFlow(t)
	f.sendErr = errors.New("socket down")
	if got := states(f.run(t)); got != "downloading verifying selftest swapped restarting" {
		t.Fatalf("states = %q", got)
	}
	if *code == -1 {
		t.Fatal("a dead socket stopped the upgrade from restarting")
	}
	if res, err := ReadOutbox(f.u.BinaryDir); res != nil || err != nil {
		t.Fatalf("outbox = %+v, %v, want empty", res, err)
	}
}

func TestUpgradeSwapsWritesMarkerAndExits(t *testing.T) {
	code := stubExit(t)
	f := newFlow(t)
	if got := states(f.run(t)); got != "downloading verifying selftest swapped restarting" {
		t.Fatalf("states = %q", got)
	}
	want := 0
	if runtime.GOOS == "windows" {
		want = 1
	}
	if *code != want {
		t.Fatalf("exit code = %d, want %d", *code, want)
	}
	if got, _ := os.ReadFile(f.u.BinaryPath + ".old"); string(got) != "current-binary" {
		t.Fatalf(".old = %q", got)
	}
	if info, err := os.Stat(f.u.BinaryPath); err != nil || info.Size() != f.req.SizeBytes {
		t.Fatalf("new binary not in place: %v", err)
	}
	p, err := ReadPending(filepath.Join(f.u.BinaryDir, PendingFile))
	if err != nil || p == nil {
		t.Fatalf("marker: %+v, %v", p, err)
	}
	if p.FromVersion != buildinfo.Version || p.ToVersion != "0.4.0" || p.UpgradeID != testUpgradeID ||
		p.Starts != 0 || p.RollbackReason != "" || p.StartedAt.IsZero() {
		t.Fatalf("marker %+v", p)
	}
	// The selftest child saw the running agent's config path.
	if args, _ := os.ReadFile(os.Getenv("KB_SELFTEST_ARGS")); string(args) != "selftest\n-config\n/x.yaml" {
		t.Fatalf("selftest args = %q", args)
	}
}

// Without a marker the new binary would start with no probation and no way
// back, so a marker that cannot be written undoes the swap.
func TestMarkerWriteFailureUndoesSwap(t *testing.T) {
	code := stubExit(t)
	f := newFlow(t)
	marker := filepath.Join(f.u.BinaryDir, PendingFile)
	// A non-empty directory where the marker goes: it cannot be written.
	if err := os.MkdirAll(filepath.Join(marker, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	results := f.run(t)
	if got := states(results); got != "downloading verifying selftest failed" {
		t.Fatalf("states = %q", got)
	}
	last := results[len(results)-1]
	if last.Reason != proto.UpgradeReasonSwapFailed {
		t.Fatalf("reason = %q, want swap_failed", last.Reason)
	}
	if !strings.HasPrefix(last.Detail, "write upgrade_pending.json: ") || !strings.HasSuffix(last.Detail, "; swap undone") {
		t.Fatalf("detail = %q", last.Detail)
	}
	if *code != -1 {
		t.Fatalf("agent exited (%d) to restart into a binary with no marker", *code)
	}
	if got, _ := os.ReadFile(f.u.BinaryPath); string(got) != "current-binary" {
		t.Fatalf("swap not undone: installed binary is %d bytes", len(got))
	}
	for _, leftover := range []string{f.u.BinaryPath + ".old", f.u.BinaryPath + ".failed"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("%s left behind (stat err %v)", filepath.Base(leftover), err)
		}
	}
}
