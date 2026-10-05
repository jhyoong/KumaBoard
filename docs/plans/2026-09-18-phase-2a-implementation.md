# Phase 2A: Agent Upgrades -- Agent and Shared Foundations

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build the agent-side upgrade pipeline: signing, selftest, download, verify, swap, rollback, and build tooling.

**Architecture:** The signing package (`internal/signing/`) is shared by the server CLI (sign) and the agent (verify). The upgrade module (`agent/upgrade/`) handles download, verify, selftest, swap, and rollback. Build tooling (`Makefile`, dev keys) supports the release workflow.

**Tech Stack:** Go standard library `crypto/ed25519`, `encoding/hex`, `encoding/base64`, `crypto/sha256`, `os/exec`. No new dependencies.

**Prerequisite:** None. This plan can be executed independently. Part 2B (server + dashboard) depends on tasks 1 and 4 from this plan.

---

### Task 1: Signing package

**Files:**
- Create: `internal/signing/signing.go`
- Create: `internal/signing/signing_test.go`

**Step 1: Write the failing tests**

```go
// internal/signing/signing_test.go
package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func TestSignAndVerifyRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	msg := BuildMessage("0.4.0", "linux", "amd64", "abcdef1234567890")
	sig := Sign(priv, msg)
	if !Verify(pub, msg, sig) {
		t.Fatal("valid signature rejected")
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	msg := BuildMessage("0.4.0", "linux", "amd64", "abcdef")
	sig := Sign(priv, msg)
	if Verify(pub2, msg, sig) {
		t.Fatal("wrong key accepted")
	}
}

func TestVerifyRejectsTamperedMessage(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	msg := BuildMessage("0.4.0", "linux", "amd64", "abcdef")
	sig := Sign(priv, msg)
	tampered := BuildMessage("0.5.0", "linux", "amd64", "abcdef")
	if Verify(pub, tampered, sig) {
		t.Fatal("tampered message accepted")
	}
}

func TestVerifyEitherKey(t *testing.T) {
	pub1, priv1, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	msg := BuildMessage("0.4.0", "linux", "amd64", "abc")
	sig := Sign(priv1, msg)
	if !VerifyAny([]ed25519.PublicKey{pub1, pub2}, msg, sig) {
		t.Fatal("key1 rejected when in slot")
	}
	if !VerifyAny([]ed25519.PublicKey{pub2, pub1}, msg, sig) {
		t.Fatal("key1 rejected when second in slot")
	}
	pub3, _, _ := ed25519.GenerateKey(rand.Reader)
	if VerifyAny([]ed25519.PublicKey{pub2, pub3}, msg, sig) {
		t.Fatal("neither key should match")
	}
}

func TestParseHexKey(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	hexStr := hex.EncodeToString(pub)
	got, err := ParseHexPublicKey(hexStr)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(pub) {
		t.Fatal("parsed key mismatch")
	}
	if _, err := ParseHexPublicKey("short"); err == nil {
		t.Fatal("expected error for short key")
	}
	if _, err := ParseHexPublicKey(""); err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestBuildMessage(t *testing.T) {
	got := BuildMessage("0.4.0", "linux", "amd64", "abc123")
	want := "homelab-agent\n0.4.0\nlinux\namd64\nabc123\n"
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSignatureBase64RoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	msg := BuildMessage("1.0.0", "darwin", "arm64", "deadbeef")
	sig := Sign(priv, msg)
	b64 := base64.StdEncoding.EncodeToString(sig)
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	if !Verify(pub, msg, decoded) {
		t.Fatal("base64 round-trip broke signature")
	}
}
```

**Step 2: Run tests to verify they fail**

Run: `cd /path/to/KumaBoard && go test ./internal/signing/ -v`
Expected: compilation error, package does not exist

**Step 3: Write the implementation**

```go
// internal/signing/signing.go
package signing

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
)

// BuildMessage constructs the exact byte string that is signed.
// Format: "homelab-agent\n<version>\n<os>\n<arch>\n<sha256>\n"
func BuildMessage(version, os, arch, sha256Hex string) []byte {
	return []byte(fmt.Sprintf("homelab-agent\n%s\n%s\n%s\n%s\n", version, os, arch, sha256Hex))
}

// Sign signs a message with the given private key.
func Sign(key ed25519.PrivateKey, message []byte) []byte {
	return ed25519.Sign(key, message)
}

// Verify checks a signature against one public key.
func Verify(pub ed25519.PublicKey, message, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, message, sig)
}

// VerifyAny checks a signature against any of the provided public keys.
// Returns true if any key validates the signature.
func VerifyAny(keys []ed25519.PublicKey, message, sig []byte) bool {
	for _, k := range keys {
		if Verify(k, message, sig) {
			return true
		}
	}
	return false
}

// ParseHexPublicKey decodes a hex-encoded ed25519 public key.
func ParseHexPublicKey(s string) (ed25519.PublicKey, error) {
	if s == "" {
		return nil, errors.New("signing: empty key")
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("signing: decode hex: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("signing: key is %d bytes, want %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}
```

**Step 4: Run tests to verify they pass**

Run: `cd /path/to/KumaBoard && go test ./internal/signing/ -v`
Expected: all PASS

**Step 5: Commit**

```bash
git add internal/signing/
git commit -m "feat(signing): ed25519 sign/verify shared by CLI and agent"
```

---

### Task 2: Build info key slots

**Files:**
- Modify: `internal/buildinfo/buildinfo.go`
- Modify: `Makefile`

**Step 1: Add key slot variables to buildinfo**

```go
// internal/buildinfo/buildinfo.go
package buildinfo

// Version is the agent or server version. Overridden by the Makefile.
var Version = "dev"

// ReleaseKeyCurrentHex is the current ed25519 signing public key (hex).
// Compiled in via -ldflags. Empty in dev builds.
var ReleaseKeyCurrentHex = ""

// ReleaseKeyNextHex is the next ed25519 signing public key (hex).
// Compiled in via -ldflags. Empty in dev builds.
var ReleaseKeyNextHex = ""
```

**Step 2: Update Makefile LDFLAGS to accept keys**

Add `RELEASE_KEY_CURRENT` and `RELEASE_KEY_NEXT` variables and extend `LDFLAGS`:

```makefile
VERSION ?= dev
MODULE  := github.com/jhyoong/KumaBoard
RELEASE_KEY_CURRENT ?=
RELEASE_KEY_NEXT    ?=
LDFLAGS := -s -w -X $(MODULE)/internal/buildinfo.Version=$(VERSION)
ifneq ($(RELEASE_KEY_CURRENT),)
LDFLAGS += -X $(MODULE)/internal/buildinfo.ReleaseKeyCurrentHex=$(RELEASE_KEY_CURRENT)
endif
ifneq ($(RELEASE_KEY_NEXT),)
LDFLAGS += -X $(MODULE)/internal/buildinfo.ReleaseKeyNextHex=$(RELEASE_KEY_NEXT)
endif
BIN     := bin
```

**Step 3: Verify the build still works**

Run: `cd /path/to/KumaBoard && go build ./internal/buildinfo/ && go vet ./internal/buildinfo/`
Expected: no errors

**Step 4: Commit**

```bash
git add internal/buildinfo/buildinfo.go Makefile
git commit -m "feat(buildinfo): two ed25519 release key slots via ldflags"
```

---

### Task 3: Agent selftest subcommand

**Files:**
- Modify: `cmd/kuma-agent/main.go`

**Step 1: Add selftest to the switch and usage**

In `cmd/kuma-agent/main.go`, add `selftest` to `usage()` and the switch in `main()`:

```go
// In usage():
fmt.Fprintln(os.Stderr, `usage: kuma-agent <command> [flags]

commands:
  run        connect to the control plane (flags: -config PATH)
  selftest   run local checks and exit (flags: -config PATH)
  install    install the Windows service (Windows only)
  uninstall  remove the Windows service (Windows only)
  version    print version and protocol version`)

// In main() switch:
	case "selftest":
		err = runSelftest(os.Args[2:])
```

**Step 2: Write the selftest function**

```go
func runSelftest(args []string) error {
	fs := flag.NewFlagSet("selftest", flag.ContinueOnError)
	path := fs.String("config", defaultConfigPath(), "path to config.yaml")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := config.Load(*path); err != nil {
		return fmt.Errorf("selftest: config: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := collectors.Collect(ctx); err != nil {
		return fmt.Errorf("selftest: collectors: %w", err)
	}
	fmt.Printf("kuma-agent %s protocol %d\n", buildinfo.Version, proto.Version)
	return nil
}
```

Add `"time"` and `"github.com/jhyoong/KumaBoard/agent/collectors"` to imports.

**Step 3: Verify it compiles**

Run: `cd /path/to/KumaBoard && go build -o /dev/null ./cmd/kuma-agent/`
Expected: no errors

**Step 4: Commit**

```bash
git add cmd/kuma-agent/main.go
git commit -m "feat(agent): selftest subcommand for upgrade pre-check"
```

---

### Task 4: Upgrade pending marker

**Files:**
- Create: `agent/upgrade/pending.go`
- Create: `agent/upgrade/pending_test.go`

**Step 1: Write the failing tests**

```go
// agent/upgrade/pending_test.go
package upgrade

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPendingWriteRead(t *testing.T) {
	dir := t.TempDir()
	p := &Pending{
		FromVersion:    "0.3.0",
		ToVersion:      "0.4.0",
		StartedAt:      time.Now().UTC().Truncate(time.Second),
		RollbackReason: "",
	}
	path := filepath.Join(dir, PendingFile)
	if err := WritePending(path, p); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPending(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.FromVersion != p.FromVersion || got.ToVersion != p.ToVersion {
		t.Fatalf("mismatch: %+v vs %+v", got, p)
	}
	if got.StartedAt.Unix() != p.StartedAt.Unix() {
		t.Fatalf("time mismatch")
	}
}

func TestPendingReadMissing(t *testing.T) {
	got, err := ReadPending(filepath.Join(t.TempDir(), PendingFile))
	if err != nil {
		t.Fatal("expected nil error for missing file")
	}
	if got != nil {
		t.Fatal("expected nil pending for missing file")
	}
}

func TestPendingSetReason(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PendingFile)
	p := &Pending{FromVersion: "0.3.0", ToVersion: "0.4.0", StartedAt: time.Now().UTC()}
	WritePending(path, p)
	if err := SetRollbackReason(path, "no_handshake"); err != nil {
		t.Fatal(err)
	}
	got, _ := ReadPending(path)
	if got.RollbackReason != "no_handshake" {
		t.Fatalf("reason = %q", got.RollbackReason)
	}
}

func TestPendingDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PendingFile)
	WritePending(path, &Pending{FromVersion: "a", ToVersion: "b", StartedAt: time.Now().UTC()})
	os.Remove(path)
	got, _ := ReadPending(path)
	if got != nil {
		t.Fatal("expected nil after delete")
	}
}
```

**Step 2: Run tests to verify they fail**

Run: `cd /path/to/KumaBoard && go test ./agent/upgrade/ -v -run TestPending`
Expected: compilation error

**Step 3: Write the implementation**

```go
// agent/upgrade/pending.go
package upgrade

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"
)

// PendingFile is the marker filename written to the binary directory.
const PendingFile = "upgrade_pending.json"

// Pending is the upgrade marker written before restarting.
type Pending struct {
	FromVersion    string    `json:"from_version"`
	ToVersion      string    `json:"to_version"`
	StartedAt      time.Time `json:"started_at"`
	RollbackReason string    `json:"rollback_reason"`
}

// ReadPending reads the marker file. Returns (nil, nil) if the file does not exist.
func ReadPending(path string) (*Pending, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Pending
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// WritePending writes the marker file atomically within the same directory.
func WritePending(path string, p *Pending) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// SetRollbackReason reads the marker, sets the reason, and writes it back.
func SetRollbackReason(path, reason string) error {
	p, err := ReadPending(path)
	if err != nil || p == nil {
		return err
	}
	p.RollbackReason = reason
	return WritePending(path, p)
}
```

**Step 4: Run tests to verify they pass**

Run: `cd /path/to/KumaBoard && go test ./agent/upgrade/ -v -run TestPending`
Expected: all PASS

**Step 5: Commit**

```bash
git add agent/upgrade/
git commit -m "feat(upgrade): pending marker read/write"
```

---

### Task 5: Platform swap logic

**Files:**
- Create: `agent/upgrade/swap_unix.go`
- Create: `agent/upgrade/swap_windows.go`
- Create: `agent/upgrade/swap_test.go`

**Step 1: Write the failing tests**

```go
// agent/upgrade/swap_test.go
package upgrade

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSwapCreatesOld(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "kuma-agent")
	if runtime.GOOS == "windows" {
		current = filepath.Join(dir, "kuma-agent.exe")
	}
	os.WriteFile(current, []byte("old-binary"), 0o755)
	temp := filepath.Join(dir, "kuma-agent.new")
	os.WriteFile(temp, []byte("new-binary"), 0o755)

	if err := Swap(current, temp); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(current)
	if string(got) != "new-binary" {
		t.Fatalf("current = %q", got)
	}
	old, _ := os.ReadFile(current + ".old")
	if string(old) != "old-binary" {
		t.Fatalf(".old = %q", old)
	}
}

func TestSwapOverwritesExistingOld(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "kuma-agent")
	if runtime.GOOS == "windows" {
		current = filepath.Join(dir, "kuma-agent.exe")
	}
	os.WriteFile(current, []byte("v2"), 0o755)
	os.WriteFile(current+".old", []byte("v1"), 0o755)
	temp := filepath.Join(dir, "kuma-agent.new")
	os.WriteFile(temp, []byte("v3"), 0o755)

	if err := Swap(current, temp); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(current)
	if string(got) != "v3" {
		t.Fatalf("current = %q", got)
	}
	old, _ := os.ReadFile(current + ".old")
	if string(old) != "v2" {
		t.Fatalf(".old = %q", old)
	}
}

func TestRestoreFromOld(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "kuma-agent")
	if runtime.GOOS == "windows" {
		current = filepath.Join(dir, "kuma-agent.exe")
	}
	os.WriteFile(current, []byte("bad"), 0o755)
	os.WriteFile(current+".old", []byte("good"), 0o755)

	if err := RestoreOld(current); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(current)
	if string(got) != "good" {
		t.Fatalf("current = %q", got)
	}
}
```

**Step 2: Run tests to verify they fail**

Run: `cd /path/to/KumaBoard && go test ./agent/upgrade/ -v -run TestSwap`
Expected: compilation error

**Step 3: Write Unix swap**

```go
// agent/upgrade/swap_unix.go
//go:build !windows

package upgrade

import (
	"fmt"
	"os"
)

// Swap replaces currentPath with tempPath, keeping currentPath.old as backup.
// Unix: hard-link current to .old, then atomic rename temp over current.
func Swap(currentPath, tempPath string) error {
	oldPath := currentPath + ".old"
	os.Remove(oldPath)
	// Hard-link preserves the running process's inode.
	if err := os.Link(currentPath, oldPath); err != nil {
		// Cross-device or other failure: fall back to rename.
		if err2 := os.Rename(currentPath, oldPath); err2 != nil {
			return fmt.Errorf("swap: backup current: %w", err2)
		}
	}
	if err := os.Rename(tempPath, currentPath); err != nil {
		// Attempt to undo the backup.
		os.Rename(oldPath, currentPath)
		return fmt.Errorf("swap: rename new to current: %w", err)
	}
	return nil
}

// RestoreOld puts the .old binary back in place of the current one.
func RestoreOld(currentPath string) error {
	oldPath := currentPath + ".old"
	if err := os.Rename(oldPath, currentPath); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	return nil
}

// CleanupFailed removes any .failed file left from a Windows rollback.
// No-op on Unix.
func CleanupFailed(currentPath string) {}
```

**Step 4: Write Windows swap**

```go
// agent/upgrade/swap_windows.go
//go:build windows

package upgrade

import (
	"fmt"
	"os"
)

// Swap replaces currentPath with tempPath, keeping currentPath.old as backup.
// Windows: rename current to .old, then rename temp to current.
func Swap(currentPath, tempPath string) error {
	oldPath := currentPath + ".old"
	os.Remove(oldPath)
	CleanupFailed(currentPath)
	if err := os.Rename(currentPath, oldPath); err != nil {
		return fmt.Errorf("swap: rename current to old: %w", err)
	}
	if err := os.Rename(tempPath, currentPath); err != nil {
		os.Rename(oldPath, currentPath)
		return fmt.Errorf("swap: rename new to current: %w", err)
	}
	return nil
}

// RestoreOld puts the .old binary back. On Windows the running binary must
// be renamed out of the way first.
func RestoreOld(currentPath string) error {
	failedPath := currentPath + ".failed"
	os.Remove(failedPath)
	if err := os.Rename(currentPath, failedPath); err != nil {
		return fmt.Errorf("restore: move running binary: %w", err)
	}
	oldPath := currentPath + ".old"
	if err := os.Rename(oldPath, currentPath); err != nil {
		return fmt.Errorf("restore: move old back: %w", err)
	}
	return nil
}

// CleanupFailed removes any .failed file from a previous Windows rollback.
func CleanupFailed(currentPath string) {
	os.Remove(currentPath + ".failed")
}
```

**Step 5: Run tests**

Run: `cd /path/to/KumaBoard && go test ./agent/upgrade/ -v -run "TestSwap|TestRestore"`
Expected: all PASS

**Step 6: Commit**

```bash
git add agent/upgrade/swap_unix.go agent/upgrade/swap_windows.go agent/upgrade/swap_test.go
git commit -m "feat(upgrade): platform-specific binary swap with .old backup"
```

---

### Task 6: Agent upgrade core

**Files:**
- Create: `agent/upgrade/upgrade.go`
- Create: `agent/upgrade/upgrade_test.go`

**Step 1: Write the upgrade_test.go tests**

Key tests to write (these test download + verify + selftest logic using
`net/http/httptest`):

- `TestVerifySize` - rejects size mismatch
- `TestVerifySHA256` - rejects hash mismatch
- `TestVerifySignature` - rejects wrong signature, accepts correct one
- `TestVerifyWrongArch` - signature for different arch is rejected
- `TestDownload` - downloads from httptest server, verifies token in request headers

```go
// agent/upgrade/upgrade_test.go  (add to existing file)
package upgrade

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

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

	// Write a temp file to verify against.
	tmp := filepath.Join(u.BinaryDir, "kuma-agent.new")
	os.WriteFile(tmp, data, 0o755)

	req := proto.UpgradeRequest{
		Version: "0.4.0", OS: runtime.GOOS, Arch: runtime.GOARCH,
		SHA256: sha, SizeBytes: int64(len(data)), Signature: sig,
	}
	if err := u.verifyArtifact(tmp, req); err != nil {
		t.Fatalf("valid artifact rejected: %v", err)
	}

	// Wrong size.
	badReq := req
	badReq.SizeBytes = 999
	if err := u.verifyArtifact(tmp, badReq); err == nil {
		t.Fatal("wrong size accepted")
	}

	// Wrong hash.
	badReq = req
	badReq.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := u.verifyArtifact(tmp, badReq); err == nil {
		t.Fatal("wrong hash accepted")
	}

	// Wrong signature.
	badReq = req
	badReq.Signature = base64.StdEncoding.EncodeToString([]byte("bad-sig-that-is-not-64-bytes-but-will-fail-verify"))
	if err := u.verifyArtifact(tmp, badReq); err == nil {
		t.Fatal("wrong signature accepted")
	}

	// Correct signature but wrong version in request.
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
```

**Step 2: Run tests to verify they fail**

Run: `cd /path/to/KumaBoard && go test ./agent/upgrade/ -v -run "TestVerifyArtifact|TestDownload"`
Expected: compilation error

**Step 3: Write the implementation**

```go
// agent/upgrade/upgrade.go
package upgrade

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/jhyoong/KumaBoard/internal/buildinfo"
	"github.com/jhyoong/KumaBoard/internal/signing"
	"github.com/jhyoong/KumaBoard/proto"
)

// Sender is the function the upgrader calls to report state.
type Sender func(proto.UpgradeResult) error

// Upgrader handles upgrade requests.
type Upgrader struct {
	BinaryPath string // os.Executable() result
	BinaryDir  string // filepath.Dir(BinaryPath)
	OS         string
	Arch       string
	Keys       []ed25519.PublicKey
	DeviceName string
	Token      string
	HTTPClient *http.Client // uses pinned CA in production
	Log        *slog.Logger

	mu sync.Mutex
}

// New creates an Upgrader from the running agent's state.
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

// HandleUpgradeRequest runs the full upgrade flow. It blocks until
// the upgrade either fails or the process exits. Call in a goroutine.
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

	// 1. Download
	report(proto.UpgradeDownloading, "")
	tmpPath, err := u.download(req.URL)
	if err != nil {
		u.Log.Error("upgrade download failed", "err", err)
		report(proto.UpgradeFailed, proto.UpgradeReasonDownloadFailed)
		return
	}

	cleanup := func() { os.Remove(tmpPath) }

	// 2. Verify
	report(proto.UpgradeVerifying, "")
	if err := u.verifyArtifact(tmpPath, req); err != nil {
		u.Log.Error("upgrade verify failed", "err", err)
		cleanup()
		report(proto.UpgradeFailed, proto.UpgradeReasonVerifyFailed)
		return
	}

	// 3. Selftest
	report(proto.UpgradeSelftest, "")
	if err := u.runSelftest(tmpPath); err != nil {
		u.Log.Error("upgrade selftest failed", "err", err)
		cleanup()
		report(proto.UpgradeFailed, proto.UpgradeReasonSelftestFailed)
		return
	}

	// 4. Swap
	if err := Swap(u.BinaryPath, tmpPath); err != nil {
		u.Log.Error("upgrade swap failed", "err", err)
		report(proto.UpgradeFailed, proto.UpgradeReasonSwapFailed)
		return
	}
	report(proto.UpgradeSwapped, "")

	// 5. Write marker
	marker := filepath.Join(u.BinaryDir, PendingFile)
	WritePending(marker, &Pending{
		FromVersion: from,
		ToVersion:   req.Version,
		StartedAt:   time.Now().UTC(),
	})

	// 6. Report restarting and exit
	report(proto.UpgradeRestarting, "")
	u.Log.Info("upgrade: restarting", "from", from, "to", req.Version)
	if runtime.GOOS == "windows" {
		os.Exit(1) // SCM restarts via failure actions
	} else {
		os.Exit(0) // systemd/launchd restart on any exit
	}
}

func (u *Upgrader) download(url string) (string, error) {
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
	tmp, err := os.CreateTemp(u.BinaryDir, "kuma-agent-upgrade-*")
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

func (u *Upgrader) runSelftest(binaryPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binaryPath, "selftest")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
```

Add `"context"` to the imports.

**Step 4: Run tests**

Run: `cd /path/to/KumaBoard && go test ./agent/upgrade/ -v -run "TestVerifyArtifact|TestDownload"`
Expected: all PASS

**Step 5: Commit**

```bash
git add agent/upgrade/upgrade.go agent/upgrade/upgrade_test.go
git commit -m "feat(upgrade): download, verify, selftest, swap, exit"
```

---

### Task 7: Rollback on startup

**Files:**
- Create: `agent/upgrade/rollback.go`
- Add tests to: `agent/upgrade/pending_test.go`

**Step 1: Write rollback types and startup check**

```go
// agent/upgrade/rollback.go
package upgrade

import (
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jhyoong/KumaBoard/internal/buildinfo"
	"github.com/jhyoong/KumaBoard/proto"
)

// StartupState is the result of checking upgrade_pending.json at startup.
type StartupState int

const (
	// StateNormal means no pending upgrade.
	StateNormal StartupState = iota
	// StateProbation means this is the new binary on its first run.
	StateProbation
	// StateRolledBack means the old binary is running after a rollback.
	StateRolledBack
)

// StartupResult is returned by CheckStartup.
type StartupResult struct {
	State          StartupState
	PendingMarker  string // path to upgrade_pending.json
	RollbackReason string // set when State == StateRolledBack
	FromVersion    string
	ToVersion      string
}

// CheckStartup reads upgrade_pending.json and determines the agent's state.
// It handles the stale-marker case internally by deleting the file and
// returning StateNormal.
func CheckStartup(binaryDir string, log *slog.Logger) StartupResult {
	markerPath := filepath.Join(binaryDir, PendingFile)
	p, err := ReadPending(markerPath)
	if err != nil {
		log.Warn("upgrade: failed to read pending marker", "err", err)
		os.Remove(markerPath)
		return StartupResult{State: StateNormal}
	}
	if p == nil {
		CleanupFailed(filepath.Join(binaryDir, "kuma-agent"))
		return StartupResult{State: StateNormal}
	}

	version := buildinfo.Version
	switch {
	case version == p.ToVersion:
		log.Info("upgrade: probation started", "from", p.FromVersion, "to", p.ToVersion)
		return StartupResult{
			State: StateProbation, PendingMarker: markerPath,
			FromVersion: p.FromVersion, ToVersion: p.ToVersion,
		}
	case version == p.FromVersion:
		log.Info("upgrade: running after rollback", "from", p.FromVersion, "to", p.ToVersion, "reason", p.RollbackReason)
		return StartupResult{
			State: StateRolledBack, PendingMarker: markerPath,
			RollbackReason: p.RollbackReason,
			FromVersion: p.FromVersion, ToVersion: p.ToVersion,
		}
	default:
		log.Warn("upgrade: stale marker, deleting", "marker_to", p.ToVersion, "own_version", version)
		os.Remove(markerPath)
		return StartupResult{State: StateNormal}
	}
}

// ConfirmUpgrade is called when probation succeeds (handshake within 120s).
// It deletes the marker and .old files.
func ConfirmUpgrade(binaryDir, binaryPath string, log *slog.Logger) {
	os.Remove(filepath.Join(binaryDir, PendingFile))
	os.Remove(binaryPath + ".old")
	log.Info("upgrade: confirmed, marker and .old removed")
}

// Rollback is called when probation fails (no handshake within 120s).
// It writes the reason, restores .old, and exits.
func Rollback(binaryDir, binaryPath, markerPath string, log *slog.Logger) {
	SetRollbackReason(markerPath, proto.UpgradeReasonNoHandshake)
	if err := RestoreOld(binaryPath); err != nil {
		log.Error("upgrade: rollback failed", "err", err)
	}
	log.Error("upgrade: rolling back, no handshake within timeout")
	os.Exit(1)
}

// ProbationTimeout is how long to wait for a handshake before rolling back.
const ProbationTimeout = 120 * time.Second
```

**Step 2: Write tests for CheckStartup**

```go
// Add to agent/upgrade/pending_test.go

func TestCheckStartupNormal(t *testing.T) {
	dir := t.TempDir()
	result := CheckStartup(dir, slog.Default())
	if result.State != StateNormal {
		t.Fatalf("expected normal, got %d", result.State)
	}
}

func TestCheckStartupStaleMarker(t *testing.T) {
	dir := t.TempDir()
	WritePending(filepath.Join(dir, PendingFile), &Pending{
		FromVersion: "0.1.0", ToVersion: "0.2.0", StartedAt: time.Now().UTC(),
	})
	// buildinfo.Version is "dev", which matches neither.
	result := CheckStartup(dir, slog.Default())
	if result.State != StateNormal {
		t.Fatalf("expected stale marker removed, got %d", result.State)
	}
	// Marker should be deleted.
	if _, err := os.Stat(filepath.Join(dir, PendingFile)); err == nil {
		t.Fatal("stale marker not deleted")
	}
}
```

**Step 3: Run tests**

Run: `cd /path/to/KumaBoard && go test ./agent/upgrade/ -v -run TestCheckStartup`
Expected: all PASS

**Step 4: Commit**

```bash
git add agent/upgrade/rollback.go agent/upgrade/pending_test.go
git commit -m "feat(upgrade): rollback logic and startup state check"
```

---

### Task 8: Agent app integration

**Files:**
- Modify: `agent/app/app.go`
- Modify: `cmd/kuma-agent/main.go`

**Step 1: Add upgrade handling to OnMessage**

In `agent/app/app.go`, add the `upgrade` package import and an `upgrader` field
to `App`. Handle `TypeUpgradeRequest` in `OnMessage`:

```go
// New field in App struct:
	upgrader *upgrade.Upgrader
	startup  upgrade.StartupResult

// In OnMessage, add a case:
	case proto.TypeUpgradeRequest:
		var req proto.UpgradeRequest
		if err := env.Unmarshal(&req); err != nil {
			return
		}
		go a.upgrader.HandleUpgradeRequest(req, func(res proto.UpgradeResult) error {
			e, err := proto.New(proto.TypeUpgradeResult, res)
			if err != nil {
				return err
			}
			return send.Send(e)
		})
```

**Step 2: Add probation handling to OnConnected**

In `OnConnected`, after the existing logic, check `a.startup`:

```go
	// After existing OnConnected logic:
	switch a.startup.State {
	case upgrade.StateProbation:
		upgrade.ConfirmUpgrade(a.upgrader.BinaryDir, a.upgrader.BinaryPath, a.log)
		if a.probationTimer != nil {
			a.probationTimer.Stop()
		}
		res := proto.UpgradeResult{
			FromVersion: a.startup.FromVersion,
			ToVersion:   a.startup.ToVersion,
			State:       proto.UpgradeVerified,
		}
		if e, err := proto.New(proto.TypeUpgradeResult, res); err == nil {
			send.Send(e)
		}
		a.startup.State = upgrade.StateNormal
	case upgrade.StateRolledBack:
		res := proto.UpgradeResult{
			FromVersion: a.startup.FromVersion,
			ToVersion:   a.startup.ToVersion,
			State:       proto.UpgradeRolledBack,
			Reason:      a.startup.RollbackReason,
		}
		if e, err := proto.New(proto.TypeUpgradeResult, res); err == nil {
			send.Send(e)
		}
		os.Remove(a.startup.PendingMarker)
		a.startup.State = upgrade.StateNormal
	}
```

**Step 3: Wire Upgrader creation in New and startup check in runAgent**

In `cmd/kuma-agent/main.go` `runAgent`, before `runWithConfig`:

```go
	// Determine binary path and dir.
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine executable path: %w", err)
	}
	binaryDir := filepath.Dir(exe)
	startupResult := upgrade.CheckStartup(binaryDir, log)
```

Pass `startupResult` and `exe` through to `app.New` or set them on the `App`
after creation.

In `app.New`, create the `Upgrader`:

```go
func New(cfg *config.Config, log *slog.Logger, startupResult upgrade.StartupResult) *App {
	exe, _ := os.Executable()
	workDir := filepath.Dir(exe)
	keys := parseKeys()
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: cfg.CAPool, MinVersion: tls.VersionTLS12}},
	}
	a := &App{
		cfg:     cfg,
		log:     log,
		runner:  commands.New(cfg.Commands, workDir, log),
		startup: startupResult,
		upgrader: upgrade.New(exe, keys, cfg.Device.Name, cfg.Token, client, log),
	}
	if startupResult.State == upgrade.StateProbation {
		a.probationTimer = time.AfterFunc(upgrade.ProbationTimeout, func() {
			upgrade.Rollback(workDir, exe, startupResult.PendingMarker, log)
		})
	}
	return a
}

func parseKeys() []ed25519.PublicKey {
	var keys []ed25519.PublicKey
	for _, hex := range []string{buildinfo.ReleaseKeyCurrentHex, buildinfo.ReleaseKeyNextHex} {
		if hex == "" {
			continue
		}
		k, err := signing.ParseHexPublicKey(hex)
		if err == nil {
			keys = append(keys, k)
		}
	}
	return keys
}
```

**Step 4: Update runWithConfig signature**

`runWithConfig` needs the startup result:

```go
func runWithConfig(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	startupResult := upgrade.CheckStartup(filepath.Dir(mustExe()), log)
	a := app.New(cfg, log, startupResult)
	// ... rest unchanged
}

func mustExe() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return exe
}
```

**Step 5: Verify everything compiles**

Run: `cd /path/to/KumaBoard && go build ./cmd/kuma-agent/`
Expected: no errors

Run: `cd /path/to/KumaBoard && go test ./...`
Expected: all PASS

**Step 6: Commit**

```bash
git add agent/app/app.go cmd/kuma-agent/main.go
git commit -m "feat(agent): wire upgrade handler and rollback into app lifecycle"
```

---
### Task 15: Makefile release target

**Files:**
- Modify: `Makefile`

**Step 1: Add the release target**

```makefile
RELEASES := releases

.PHONY: release

release:
	@test -n "$(VERSION)" || (echo "VERSION is required: make release VERSION=x.y.z"; exit 1)
	@test "$(VERSION)" != "dev" || (echo "VERSION must not be dev"; exit 1)
	@mkdir -p $(RELEASES)/$(VERSION)/linux_amd64 $(RELEASES)/$(VERSION)/linux_arm64 \
	          $(RELEASES)/$(VERSION)/darwin_arm64 $(RELEASES)/$(VERSION)/windows_amd64
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(RELEASES)/$(VERSION)/linux_amd64/kuma-agent   ./cmd/kuma-agent
	GOOS=linux   GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(RELEASES)/$(VERSION)/linux_arm64/kuma-agent   ./cmd/kuma-agent
	GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(RELEASES)/$(VERSION)/darwin_arm64/kuma-agent  ./cmd/kuma-agent
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(RELEASES)/$(VERSION)/windows_amd64/kuma-agent.exe ./cmd/kuma-agent
	@cd $(RELEASES)/$(VERSION) && \
	  echo '{"version":"$(VERSION)","artifacts":[' > manifest.json && \
	  for d in linux_amd64 linux_arm64 darwin_arm64 windows_amd64; do \
	    os=$$(echo $$d | cut -d_ -f1); \
	    arch=$$(echo $$d | cut -d_ -f2); \
	    bin=$$d/kuma-agent; \
	    [ "$$os" = "windows" ] && bin=$$d/kuma-agent.exe; \
	    sha=$$(shasum -a 256 $$bin | cut -d' ' -f1); \
	    size=$$(stat -f%z $$bin 2>/dev/null || stat --printf=%s $$bin); \
	    printf '{"os":"%s","arch":"%s","sha256":"%s","size_bytes":%s,"signature":""}' "$$os" "$$arch" "$$sha" "$$size"; \
	    [ "$$d" != "windows_amd64" ] && printf ','; \
	  done >> manifest.json && \
	  echo ']}' >> manifest.json
	@echo "Release $(VERSION) built in $(RELEASES)/$(VERSION)/"
```

**Step 2: Add `releases/` to .gitignore**

```
# Release artifacts
releases/
```

**Step 3: Verify it compiles**

Run: `cd /path/to/KumaBoard && make release VERSION=0.0.1-test 2>&1 | tail -5`
Expected: builds succeed, manifest.json written (clean up after: `rm -rf releases/0.0.1-test`)

**Step 4: Commit**

```bash
git add Makefile .gitignore
git commit -m "build: make release target with manifest generation"
```

---

### Task 18: Dev key generation and documentation

**Files:**
- Create: `configs/dev-release-keys.txt`
- Modify: `docs/plans/2026-09-18-phase-2-design.md` (note dev keys)

**Step 1: Generate a dev keypair**

Write a small Go program or use `go run` inline to generate a test ed25519
keypair and write the hex-encoded public key and raw private key:

```bash
cd /path/to/KumaBoard
go run -v <<'EOF' > configs/dev-release-keys.txt
package main
import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
)
func main() {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	fmt.Println("# Dev release signing keys (NOT for production)")
	fmt.Println("# Public (current):", hex.EncodeToString(pub))
	fmt.Println("# Public (next):    <empty for now>")
	fmt.Println("RELEASE_KEY_CURRENT=" + hex.EncodeToString(pub))
	fmt.Println("RELEASE_KEY_NEXT=")
	os.WriteFile("configs/dev-release.key", priv, 0o600)
}
EOF
```

**Step 2: Commit**

```bash
git add configs/dev-release-keys.txt
# Do NOT commit dev-release.key; add it to .gitignore
echo "configs/dev-release.key" >> .gitignore
git add .gitignore
git commit -m "build: dev release signing keypair for local testing"
```

---

## Build order summary

| Task | Depends on | What it delivers |
|---|---|---|
| 1. internal/signing | nothing | Sign/verify functions |
| 2. buildinfo key slots | nothing | LDFLAGS key injection |
| 3. agent selftest | nothing | `kuma-agent selftest` |
| 4. upgrade pending marker | nothing | Read/write upgrade_pending.json |
| 5. platform swap | 4 | Binary swap with .old |
| 6. upgrade core | 1, 4, 5 | Download, verify, selftest, swap |
| 7. rollback | 4, 5 | Startup check, probation, rollback |
| 8. agent app integration | 2, 3, 6, 7 | Wire into transport handler |
| 15. Makefile release | nothing | Build and manifest generation |
| 18. dev keys | nothing | Test keypair for development |

Tasks 1-5, 15, 18 have no dependencies on each other and can be parallelized.
