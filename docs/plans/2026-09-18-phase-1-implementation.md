# Phase 1 Foundation Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build the KumaBoard control plane server, the generic agent, and the dashboard so that seven devices connect over pinned TLS, report metrics, run named commands, and can be woken and put to sleep, with login, audit, service packaging, and nightly backup.

**Architecture:** One Go module with two binaries. `kuma-agent` dials outbound to `kumaboard` over `wss://` with a pinned CA and a per-device token. The server keeps device state in SQLite plus an in-memory registry, and pushes changes to the React dashboard over Server-Sent Events. Build order is a thin vertical slice first (protocol, handshake, one agent, one page), then widen.

**Tech Stack:** Go 1.26, `github.com/coder/websocket`, `modernc.org/sqlite`, `golang.org/x/crypto/argon2`, `github.com/oklog/ulid/v2`, `github.com/shirou/gopsutil/v4`, `golang.org/x/sys/windows/svc`, `gopkg.in/yaml.v3`, `golang.org/x/term`. Frontend: Vite 6, React 19, TypeScript, Tailwind 4, react-router 7, Vitest.

**Design reference:** `docs/plans/2026-09-18-phase-1-design.md`. Read it before starting. Sections are referenced as "design §N".

**Conventions for every task:**

- Run `gofmt -l .` and `go vet ./...` before each commit. Both must print nothing.
- All times stored in SQLite are RFC 3339 UTC strings. All times in JSON are RFC 3339.
- No mock data, no fake agents. Tests run real code against temp directories and loopback ports.
- Never log or store a plaintext device token. `proto.Hello.Redacted()` exists for this.
- Commit after every task with the message given. Append the attribution line from the session reminder to each commit message.
- Test commands run from the repo root `/path/to/KumaBoard` unless stated.

---

## Build step 1: Module skeleton and `proto`

### Task 1: Module skeleton, Makefile, buildinfo

**Files:**
- Create: `go.mod`
- Create: `Makefile`
- Create: `internal/buildinfo/buildinfo.go`
- Create: `web/dist/.gitkeep`
- Modify: `.gitignore`

**Step 1: Initialise the module**

```bash
cd /path/to/KumaBoard
go mod init github.com/jhyoong/KumaBoard
```

Expected: `go.mod` created with `go 1.26`.

**Step 2: Create buildinfo**

`internal/buildinfo/buildinfo.go`:

```go
// Package buildinfo holds values stamped in at build time via -ldflags.
package buildinfo

// Version is the agent or server version. Overridden by the Makefile.
var Version = "dev"
```

**Step 3: Create the Makefile**

`Makefile`:

```makefile
VERSION ?= dev
MODULE  := github.com/jhyoong/KumaBoard
LDFLAGS := -s -w -X $(MODULE)/internal/buildinfo.Version=$(VERSION)
BIN     := bin

export CGO_ENABLED=0

.PHONY: all test vet fmt web server agent-all \
        agent-linux-amd64 agent-linux-arm64 agent-darwin-arm64 agent-windows-amd64

all: test server agent-all

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l .

web:
	cd web && npm ci && npm run build

server: web
	go build -ldflags "$(LDFLAGS)" -o $(BIN)/kumaboard ./cmd/kumaboard

agent-all: agent-linux-amd64 agent-linux-arm64 agent-darwin-arm64 agent-windows-amd64

agent-linux-amd64:
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BIN)/kuma-agent_linux_amd64 ./cmd/kuma-agent

agent-linux-arm64:
	GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BIN)/kuma-agent_linux_arm64 ./cmd/kuma-agent

agent-darwin-arm64:
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BIN)/kuma-agent_darwin_arm64 ./cmd/kuma-agent

agent-windows-amd64:
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BIN)/kuma-agent_windows_amd64.exe ./cmd/kuma-agent

-include deploy/hosts.mk
```

Note the recipe lines must be indented with a tab, not spaces.

**Step 4: Placeholder for the embedded frontend**

```bash
mkdir -p web/dist && touch web/dist/.gitkeep
```

Append to `.gitignore`:

```
# Build output
bin/
web/node_modules/
web/dist/*
!web/dist/.gitkeep
```

**Step 5: Verify**

Run: `go vet ./... && echo ok`
Expected: `ok`

**Step 6: Commit**

```bash
git add go.mod Makefile internal/buildinfo/buildinfo.go web/dist/.gitkeep .gitignore
git commit -m "chore: module skeleton, Makefile, buildinfo"
```

---

### Task 2: `proto` version and envelope

**Files:**
- Create: `proto/version.go`
- Create: `proto/envelope.go`
- Test: `proto/envelope_test.go`

**Step 1: Write the failing tests**

`proto/envelope_test.go`:

```go
package proto

import (
	"strings"
	"testing"
)

func TestSupported(t *testing.T) {
	cases := map[int]bool{0: false, 1: true, 2: false}
	for v, want := range cases {
		if got := Supported(v); got != want {
			t.Errorf("Supported(%d) = %v, want %v", v, got, want)
		}
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	type payload struct {
		A string `json:"a"`
	}
	env, err := New("hello", payload{A: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if env.V != Version || env.Type != "hello" || len(env.ID) != 26 || env.TS.IsZero() {
		t.Fatalf("bad envelope: %+v", env)
	}
	b, err := Encode(env)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	var p payload
	if err := got.Unmarshal(&p); err != nil {
		t.Fatal(err)
	}
	if got.ID != env.ID || p.A != "x" {
		t.Fatalf("round trip mismatch: %+v %+v", got, p)
	}
}

func TestReplyEchoesID(t *testing.T) {
	req, _ := New("ping", nil)
	rep, err := Reply(req, "pong", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ID != req.ID || rep.Type != "pong" {
		t.Fatalf("reply did not echo id: %+v", rep)
	}
}

func TestDecodeRejectsOversize(t *testing.T) {
	big := []byte(`{"v":1,"id":"x","type":"t","payload":"` + strings.Repeat("a", MaxMessageSize) + `"}`)
	if _, err := Decode(big); err != ErrMessageTooLarge {
		t.Fatalf("want ErrMessageTooLarge, got %v", err)
	}
}

func TestDecodeRejectsMissingType(t *testing.T) {
	if _, err := Decode([]byte(`{"v":1,"id":"x"}`)); err == nil {
		t.Fatal("want error for missing type")
	}
}
```

**Step 2: Run to verify failure**

Run: `go test ./proto/`
Expected: FAIL, undefined: Supported, New, etc.

**Step 3: Implement**

`proto/version.go`:

```go
// Package proto defines the wire protocol shared by kumaboard and kuma-agent.
// It has no dependencies on either binary.
package proto

// Version is the protocol version this build speaks.
const Version = 1

// MinSupported is the oldest protocol version the server accepts.
// The rule is N and N-1: MinSupported is never more than one behind Version.
const MinSupported = 1

// Supported reports whether a peer's protocol version is accepted.
func Supported(v int) bool {
	return v >= MinSupported && v <= Version
}
```

`proto/envelope.go`:

```go
package proto

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
)

// MaxMessageSize is the largest encoded envelope either side accepts.
const MaxMessageSize = 1 << 20

// ErrMessageTooLarge is returned when an encoded envelope exceeds MaxMessageSize.
var ErrMessageTooLarge = errors.New("proto: message exceeds 1 MiB")

// Envelope wraps every message on the control socket.
type Envelope struct {
	V       int             `json:"v"`
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	TS      time.Time       `json:"ts"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// New builds an envelope with a fresh ULID and the current time.
func New(typ string, payload any) (*Envelope, error) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("proto: marshal payload: %w", err)
		}
		raw = b
	}
	return &Envelope{
		V:       Version,
		ID:      ulid.Make().String(),
		Type:    typ,
		TS:      time.Now().UTC(),
		Payload: raw,
	}, nil
}

// Reply builds an envelope whose ID echoes the request so the two correlate.
func Reply(to *Envelope, typ string, payload any) (*Envelope, error) {
	e, err := New(typ, payload)
	if err != nil {
		return nil, err
	}
	e.ID = to.ID
	return e, nil
}

// Encode serialises an envelope and enforces MaxMessageSize.
func Encode(e *Envelope) ([]byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("proto: encode: %w", err)
	}
	if len(b) > MaxMessageSize {
		return nil, ErrMessageTooLarge
	}
	return b, nil
}

// Decode parses an envelope and enforces MaxMessageSize.
// Callers must also set the WebSocket read limit to MaxMessageSize so an
// oversized frame is rejected before it is buffered in full.
func Decode(b []byte) (*Envelope, error) {
	if len(b) > MaxMessageSize {
		return nil, ErrMessageTooLarge
	}
	var e Envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("proto: decode: %w", err)
	}
	if e.Type == "" {
		return nil, errors.New("proto: missing type")
	}
	return &e, nil
}

// Unmarshal decodes the payload into v.
func (e *Envelope) Unmarshal(v any) error {
	if len(e.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(e.Payload, v)
}
```

**Step 4: Fetch the dependency and run tests**

```bash
go get github.com/oklog/ulid/v2@latest
go test ./proto/
```

Expected: `ok  	github.com/jhyoong/KumaBoard/proto`

**Step 5: Commit**

```bash
git add proto/ go.mod go.sum
git commit -m "feat(proto): envelope, version negotiation, size limit"
```

---

### Task 3: `proto` message types

**Files:**
- Create: `proto/messages.go`
- Create: `proto/upgrade.go`
- Create: `proto/terminal.go`
- Test: `proto/messages_test.go`

**Step 1: Write the failing test**

`proto/messages_test.go`:

```go
package proto

import "testing"

func TestHelloRedacted(t *testing.T) {
	h := Hello{DeviceName: "d", Token: "secret"}
	r := h.Redacted()
	if r.Token != "" || h.Token != "secret" || r.DeviceName != "d" {
		t.Fatalf("redaction wrong: %+v %+v", h, r)
	}
}

func TestHelloRoundTrip(t *testing.T) {
	in := Hello{
		ProtocolVersion: 1, AgentVersion: "0.1.0", DeviceName: "macos-desktop",
		Token: "t", OS: "darwin", Arch: "arm64",
		Capabilities: []string{"metrics"},
		Commands:     []CommandDef{{Name: "sleep", TimeoutS: 15, ExpectDisconnect: true}},
	}
	env, err := New(TypeHello, in)
	if err != nil {
		t.Fatal(err)
	}
	var out Hello
	if err := env.Unmarshal(&out); err != nil {
		t.Fatal(err)
	}
	if out.Commands[0].ExpectDisconnect != true || out.Capabilities[0] != "metrics" {
		t.Fatalf("round trip lost data: %+v", out)
	}
}

func TestRunStatusIsTerminal(t *testing.T) {
	if IsTerminalRunStatus(RunRunning) || IsTerminalRunStatus(RunDispatched) {
		t.Fatal("running and dispatched are not terminal")
	}
	if !IsTerminalRunStatus(RunOK) || !IsTerminalRunStatus(RunLost) {
		t.Fatal("ok and lost are terminal")
	}
}
```

**Step 2: Run to verify failure**

Run: `go test ./proto/`
Expected: FAIL, undefined: Hello

**Step 3: Implement**

`proto/messages.go`:

```go
package proto

import "time"

// Message types.
const (
	TypeHello              = "hello"
	TypeHelloAck           = "hello_ack"
	TypePing               = "ping"
	TypePong               = "pong"
	TypeMetrics            = "metrics"
	TypeCommandRequest     = "command_request"
	TypeCommandResult      = "command_result"
	TypeCommandOutput      = "command_output"
	TypeTerminalOpen       = "terminal_open"
	TypeTerminalOpenResult = "terminal_open_result"
	TypeWolRequest         = "wol_request"
	TypeUpgradeRequest     = "upgrade_request"
	TypeUpgradeResult      = "upgrade_result"
	TypeError              = "error"
)

// Error codes carried by TypeError.
const (
	ErrAuthFailed                 = "auth_failed"
	ErrUnknownDevice              = "unknown_device"
	ErrProtocolVersionUnsupported = "protocol_version_unsupported"
	ErrDuplicateSession           = "duplicate_session"
	ErrProtocol                   = "protocol_error"
)

// Command run statuses. Stored in command_runs.status and sent in CommandResult.
const (
	RunRunning                = "running"
	RunOK                     = "ok"
	RunFailed                 = "failed"
	RunTimeout                = "timeout"
	RunUnknownCommand         = "unknown_command"
	RunBusy                   = "busy"
	RunDispatched             = "dispatched"
	RunDisconnectedAsExpected = "disconnected_as_expected"
	RunNoDisconnect           = "no_disconnect"
	RunLost                   = "lost"
)

// IsTerminalRunStatus reports whether a run status will not change again.
func IsTerminalRunStatus(s string) bool {
	return s != RunRunning && s != RunDispatched
}

// Capabilities an agent may declare.
const (
	CapMetrics        = "metrics"
	CapCustomCommands = "custom-commands"
	CapWolTarget      = "wol-target"
	CapWolSender      = "wol-sender"
	CapTerminal       = "terminal"
)

// CommandDef is a command the agent declares at handshake. Untrusted input.
type CommandDef struct {
	Name             string `json:"name"`
	Description      string `json:"description"`
	TimeoutS         int    `json:"timeout_s"`
	ExpectDisconnect bool   `json:"expect_disconnect"`
}

// Hello is the agent's first message.
type Hello struct {
	ProtocolVersion int          `json:"protocol_version"`
	AgentVersion    string       `json:"agent_version"`
	DeviceName      string       `json:"device_name"`
	Token           string       `json:"token"`
	OS              string       `json:"os"`
	Arch            string       `json:"arch"`
	Capabilities    []string     `json:"capabilities"`
	Commands        []CommandDef `json:"commands"`
}

// Redacted returns a copy safe for logging and audit.
func (h Hello) Redacted() Hello {
	h.Token = ""
	return h
}

// HelloAck is the server's acceptance.
type HelloAck struct {
	SessionID          string    `json:"session_id"`
	ServerTime         time.Time `json:"server_time"`
	MetricsIntervalS   int       `json:"metrics_interval_s"`
	HeartbeatIntervalS int       `json:"heartbeat_interval_s"`
}

// Metrics is one sample from an agent.
type Metrics struct {
	CPUPercent      float64 `json:"cpu_percent"`
	MemTotalBytes   uint64  `json:"mem_total_bytes"`
	MemUsedBytes    uint64  `json:"mem_used_bytes"`
	MemUsedPercent  float64 `json:"mem_used_percent"`
	DiskTotalBytes  uint64  `json:"disk_total_bytes"`
	DiskUsedBytes   uint64  `json:"disk_used_bytes"`
	DiskUsedPercent float64 `json:"disk_used_percent"`
	UptimeS         uint64  `json:"uptime_s"`
	Load1           float64 `json:"load1"`
	Load5           float64 `json:"load5"`
	Load15          float64 `json:"load15"`
}

// CommandRequest names a command. Never a shell string.
type CommandRequest struct {
	Name string `json:"name"`
}

// CommandResult reports the outcome of a run.
type CommandResult struct {
	Status     string `json:"status"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Truncated  bool   `json:"truncated"`
	DurationMS int64  `json:"duration_ms"`
}

// CommandOutput is an optional streamed chunk. Defined in v1, not sent by the
// v1 agent; the server accepts it.
type CommandOutput struct {
	Seq    int    `json:"seq"`
	Stream string `json:"stream"` // "stdout" or "stderr"
	Data   string `json:"data"`
}

// Error is a coded error, usually followed by a close.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WolRequest asks a wol-sender agent to emit a magic packet. Dormant in v1.
type WolRequest struct {
	MAC       string `json:"mac"`
	Broadcast string `json:"broadcast"`
}
```

`proto/upgrade.go`:

```go
package proto

// Upgrade messages are frozen at protocol v1. Fields may be added; existing
// fields never change meaning and are never removed.

// UpgradeRequest asks the agent to move to a target version.
type UpgradeRequest struct {
	Version   string `json:"version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	Signature string `json:"signature"`
	URL       string `json:"url"`
}

// Upgrade states.
const (
	UpgradeDownloading = "downloading"
	UpgradeVerifying   = "verifying"
	UpgradeSelftest    = "selftest"
	UpgradeSwapped     = "swapped"
	UpgradeRestarting  = "restarting"
	UpgradeVerified    = "verified"
	UpgradeRolledBack  = "rolled_back"
	UpgradeFailed      = "failed"
)

// Upgrade failure reasons.
const (
	UpgradeReasonDownloadFailed = "download_failed"
	UpgradeReasonVerifyFailed   = "verify_failed"
	UpgradeReasonSelftestFailed = "selftest_failed"
	UpgradeReasonSwapFailed     = "swap_failed"
	UpgradeReasonNoHandshake    = "no_handshake"
)

// UpgradeResult reports progress or outcome of an upgrade.
type UpgradeResult struct {
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
	State       string `json:"state"`
	Reason      string `json:"reason,omitempty"`
}
```

`proto/terminal.go`:

```go
package proto

// TerminalOpen asks the agent to dial a terminal socket. Sent from v3.
type TerminalOpen struct {
	SessionID   string `json:"session_id"`
	AgentTicket string `json:"agent_ticket"`
	Cols        int    `json:"cols"`
	Rows        int    `json:"rows"`
}

// TerminalOpenResult is "ok" or "refused".
type TerminalOpenResult struct {
	SessionID string `json:"session_id"`
	Result    string `json:"result"`
}

// TerminalHello is the agent's first text frame on /ws/terminal.
type TerminalHello struct {
	SessionID   string `json:"session_id"`
	AgentTicket string `json:"agent_ticket"`
}

// TerminalControl is a JSON text frame on either terminal socket.
type TerminalControl struct {
	Type string `json:"type"` // "resize" or "exit"
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
	Code int    `json:"code,omitempty"`
}
```

**Step 4: Run tests**

Run: `go test ./proto/`
Expected: PASS

**Step 5: Commit**

```bash
git add proto/
git commit -m "feat(proto): all v1 message types including dormant terminal and upgrade"
```

---

## Build step 2: PKI, store, server skeleton

### Task 4: `server/pki` CA and server certificate

**Files:**
- Create: `server/pki/pki.go`
- Test: `server/pki/pki_test.go`

**Step 1: Write the failing tests**

`server/pki/pki_test.go`:

```go
package pki

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func loadCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		t.Fatalf("no PEM in %s", path)
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEnsureCreatesVerifiableChain(t *testing.T) {
	dir := t.TempDir()
	if err := Ensure(dir, []string{"127.0.0.1", "kuma.local"}); err != nil {
		t.Fatal(err)
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, CAFile))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("ca.pem did not parse")
	}
	srv := loadCert(t, filepath.Join(dir, ServerCertFile))
	if _, err := srv.Verify(x509.VerifyOptions{Roots: pool, DNSName: "kuma.local"}); err != nil {
		t.Fatalf("server cert does not verify against CA: %v", err)
	}
	if len(srv.IPAddresses) != 1 || !srv.IPAddresses[0].Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("IP SAN missing: %v", srv.IPAddresses)
	}
	for _, f := range []string{caKeyFile, serverKeyFile} {
		info, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 0600", f, info.Mode().Perm())
		}
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := Ensure(dir, []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, CAFile))
	if err := Ensure(dir, []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, CAFile))
	if string(before) != string(after) {
		t.Fatal("second Ensure regenerated the CA")
	}
}

func TestReissueKeepsCAAndChangesSANs(t *testing.T) {
	dir := t.TempDir()
	if err := Ensure(dir, []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	caBefore, _ := os.ReadFile(filepath.Join(dir, CAFile))
	if err := Reissue(dir, []string{"10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	caAfter, _ := os.ReadFile(filepath.Join(dir, CAFile))
	if string(caBefore) != string(caAfter) {
		t.Fatal("Reissue changed the CA")
	}
	srv := loadCert(t, filepath.Join(dir, ServerCertFile))
	if len(srv.IPAddresses) != 1 || !srv.IPAddresses[0].Equal(net.ParseIP("10.0.0.1")) {
		t.Fatalf("new SAN not applied: %v", srv.IPAddresses)
	}
}

func TestServerCertExpiry(t *testing.T) {
	dir := t.TempDir()
	if err := Ensure(dir, []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	exp, err := ServerCertExpiry(dir)
	if err != nil {
		t.Fatal(err)
	}
	left := time.Until(exp)
	if left < 729*24*time.Hour || left > 731*24*time.Hour {
		t.Fatalf("expiry %v from now, want about 2 years", left)
	}
}

func TestTLSConfigLoads(t *testing.T) {
	dir := t.TempDir()
	if err := Ensure(dir, []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := TLSConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatal("no certificate loaded")
	}
}
```

**Step 2: Run to verify failure**

Run: `go test ./server/pki/`
Expected: FAIL, undefined: Ensure

**Step 3: Implement**

`server/pki/pki.go`:

```go
// Package pki generates and loads the self-signed CA and the server certificate.
// The CA is the only trust root agents use.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	CAFile         = "ca.pem"
	ServerCertFile = "server.pem"
	caKeyFile      = "ca.key"
	serverKeyFile  = "server.key"

	caValidity     = 10 * 365 * 24 * time.Hour
	serverValidity = 2 * 365 * 24 * time.Hour
)

// Ensure creates the CA if missing, then the server certificate if missing.
// sans are IPs or hostnames for the server certificate.
func Ensure(dir string, sans []string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if !exists(filepath.Join(dir, caKeyFile)) {
		if err := generateCA(dir); err != nil {
			return fmt.Errorf("pki: generate CA: %w", err)
		}
	}
	if !exists(filepath.Join(dir, serverKeyFile)) {
		return Reissue(dir, sans)
	}
	return nil
}

// Reissue writes a new server certificate signed by the existing CA.
func Reissue(dir string, sans []string) error {
	caCert, caKey, err := loadCA(dir)
	if err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "KumaBoard"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(serverValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, s := range sans {
		if ip := net.ParseIP(s); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, s)
		}
	}
	if len(tmpl.IPAddresses)+len(tmpl.DNSNames) == 0 {
		return errors.New("pki: at least one SAN is required")
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	return writePair(dir, ServerCertFile, serverKeyFile, der, key)
}

// TLSConfig loads the server certificate for use by an HTTPS listener.
func TLSConfig(dir string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, ServerCertFile), filepath.Join(dir, serverKeyFile))
	if err != nil {
		return nil, fmt.Errorf("pki: load server cert: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ServerCertExpiry returns the NotAfter of the current server certificate.
func ServerCertExpiry(dir string) (time.Time, error) {
	c, err := readCert(filepath.Join(dir, ServerCertFile))
	if err != nil {
		return time.Time{}, err
	}
	return c.NotAfter, nil
}

func generateCA(dir string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "KumaBoard CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	return writePair(dir, CAFile, caKeyFile, der, key)
}

func loadCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cert, err := readCert(filepath.Join(dir, CAFile))
	if err != nil {
		return nil, nil, err
	}
	kb, err := os.ReadFile(filepath.Join(dir, caKeyFile))
	if err != nil {
		return nil, nil, err
	}
	block, _ := pem.Decode(kb)
	if block == nil {
		return nil, nil, errors.New("pki: ca.key is not PEM")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func readCert(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("pki: %s is not PEM", path)
	}
	return x509.ParseCertificate(block.Bytes)
}

func writePair(dir, certName, keyName string, der []byte, key *ecdsa.PrivateKey) error {
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	if err := os.WriteFile(filepath.Join(dir, keyName), keyPEM, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, certName), certPEM, 0o644)
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		panic(err)
	}
	return n
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
```

**Step 4: Run tests**

Run: `go test ./server/pki/`
Expected: PASS

**Step 5: Commit**

```bash
git add server/pki/
git commit -m "feat(pki): CA and server certificate generation, reissue, expiry"
```

---

### Task 5: `server/store` open and migrations

**Files:**
- Create: `server/store/store.go`
- Create: `server/store/migrations/0001_init.sql`
- Test: `server/store/store_test.go`

**Step 1: Write the failing test**

`server/store/store_test.go`:

```go
package store

import (
	"context"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenCreatesSchema(t *testing.T) {
	s := openTest(t)
	want := []string{"devices", "commands", "command_runs", "terminal_sessions",
		"audit_log", "users", "sessions", "wake_jobs", "agent_upgrades", "releases"}
	for _, table := range want {
		var n int
		err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
		if err != nil || n != 1 {
			t.Errorf("table %s missing (err=%v)", table, err)
		}
	}
	var mode string
	if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q err=%v, want wal", mode, err)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s1.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second open failed: %v", err)
	}
	defer s2.Close()
	var n int
	if err := s2.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("migrations applied = %d err=%v, want 1", n, err)
	}
}
```

**Step 2: Run to verify failure**

Run: `go test ./server/store/`
Expected: FAIL, undefined: Open

**Step 3: Implement**

`server/store/migrations/0001_init.sql`:

```sql
CREATE TABLE devices (
    id                    INTEGER PRIMARY KEY,
    name                  TEXT NOT NULL UNIQUE,
    mac                   TEXT NOT NULL DEFAULT '',
    os                    TEXT NOT NULL DEFAULT '',
    arch                  TEXT NOT NULL DEFAULT '',
    token_hash            BLOB,
    capabilities_json     TEXT NOT NULL DEFAULT '[]',
    schedule_json         TEXT NOT NULL DEFAULT '{}',
    normally_off          INTEGER NOT NULL DEFAULT 0,
    terminal_enabled      INTEGER NOT NULL DEFAULT 0,
    last_seen             TEXT,
    last_disconnect_at    TEXT,
    last_reject_reason    TEXT NOT NULL DEFAULT '',
    agent_version         TEXT NOT NULL DEFAULT '',
    desired_agent_version TEXT NOT NULL DEFAULT '',
    protocol_version      INTEGER NOT NULL DEFAULT 0,
    created_at            TEXT NOT NULL
);

CREATE TABLE commands (
    device_id         INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    name              TEXT NOT NULL,
    description       TEXT NOT NULL DEFAULT '',
    timeout_s         INTEGER NOT NULL,
    expect_disconnect INTEGER NOT NULL DEFAULT 0,
    updated_at        TEXT NOT NULL,
    PRIMARY KEY (device_id, name)
);

CREATE TABLE command_runs (
    id           TEXT PRIMARY KEY,
    device_id    INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    command      TEXT NOT NULL,
    requested_by TEXT NOT NULL,
    requested_at TEXT NOT NULL,
    started_at   TEXT,
    finished_at  TEXT,
    exit_code    INTEGER,
    status       TEXT NOT NULL,
    stdout_tail  TEXT NOT NULL DEFAULT '',
    stderr_tail  TEXT NOT NULL DEFAULT '',
    truncated    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX command_runs_device_idx ON command_runs(device_id, requested_at DESC);
CREATE INDEX command_runs_status_idx ON command_runs(status);

CREATE TABLE terminal_sessions (
    id         TEXT PRIMARY KEY,
    device_id  INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    user       TEXT NOT NULL,
    started_at TEXT NOT NULL,
    ended_at   TEXT,
    bytes_in   INTEGER NOT NULL DEFAULT 0,
    bytes_out  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE audit_log (
    id     INTEGER PRIMARY KEY,
    ts     TEXT NOT NULL,
    actor  TEXT NOT NULL,
    action TEXT NOT NULL,
    target TEXT NOT NULL DEFAULT '',
    result TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT ''
);

CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TEXT NOT NULL
);

CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL
);

CREATE TABLE wake_jobs (
    id             TEXT PRIMARY KEY,
    device_id      INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    state          TEXT NOT NULL,
    attempts       INTEGER NOT NULL DEFAULT 0,
    started_at     TEXT NOT NULL,
    finished_at    TEXT,
    failure_reason TEXT NOT NULL DEFAULT ''
);

CREATE TABLE agent_upgrades (
    id             TEXT PRIMARY KEY,
    device_id      INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    from_version   TEXT NOT NULL,
    to_version     TEXT NOT NULL,
    requested_by   TEXT NOT NULL,
    started_at     TEXT NOT NULL,
    finished_at    TEXT,
    state          TEXT NOT NULL,
    failure_reason TEXT NOT NULL DEFAULT ''
);

CREATE TABLE releases (
    version    TEXT NOT NULL,
    os         TEXT NOT NULL,
    arch       TEXT NOT NULL,
    sha256     TEXT NOT NULL,
    signature  TEXT NOT NULL,
    size_bytes INTEGER NOT NULL,
    added_at   TEXT NOT NULL,
    PRIMARY KEY (version, os, arch)
);
```

`server/store/store.go`:

```go
// Package store is the SQLite persistence layer.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

var (
	ErrNotFound = errors.New("store: not found")
	ErrExists   = errors.New("store: already exists")
)

// Store wraps the database handle.
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path and applies migrations.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// One writer at a time keeps SQLite simple. Seven devices do not need more.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var version int
		if _, err := fmt.Sscanf(name, "%d_", &version); err != nil {
			return fmt.Errorf("store: bad migration name %q", name)
		}
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, nowString()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// nowString is the canonical stored time format.
func nowString() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func timeString(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s.String)
	if err != nil {
		return nil
	}
	return &t
}

func mustTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// Audit appends one audit_log row. Never fails the caller's operation: errors
// are returned so the caller can log them, but callers should not abort on them.
func (s *Store) Audit(ctx context.Context, actor, action, target, result, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (ts, actor, action, target, result, detail) VALUES (?, ?, ?, ?, ?, ?)`,
		nowString(), actor, action, target, result, detail)
	return err
}

// AuditEntry is one audit_log row.
type AuditEntry struct {
	ID     int64     `json:"id"`
	TS     time.Time `json:"ts"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target"`
	Result string    `json:"result"`
	Detail string    `json:"detail"`
}

// ListAudit returns up to limit rows with id < beforeID (0 means newest), newest first.
func (s *Store) ListAudit(ctx context.Context, limit int, beforeID int64) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if beforeID <= 0 {
		beforeID = 1<<62 - 1
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ts, actor, action, target, result, detail FROM audit_log WHERE id < ? ORDER BY id DESC LIMIT ?`,
		beforeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts string
		if err := rows.Scan(&e.ID, &ts, &e.Actor, &e.Action, &e.Target, &e.Result, &e.Detail); err != nil {
			return nil, err
		}
		e.TS = mustTime(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}
```

**Step 4: Fetch the driver and run tests**

```bash
go get modernc.org/sqlite@latest
go test ./server/store/
```

Expected: PASS

**Step 5: Commit**

```bash
git add server/store/ go.mod go.sum
git commit -m "feat(store): sqlite open, WAL, embedded migrations, audit log"
```

---

### Task 6: `server/store` devices, tokens, users, sessions

**Files:**
- Create: `server/store/devices.go`
- Create: `server/store/users.go`
- Test: `server/store/devices_test.go`

**Step 1: Write the failing tests**

`server/store/devices_test.go`:

```go
package store

import (
	"context"
	"crypto/subtle"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
)

func TestCreateDeviceReturnsTokenOnce(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, plain, err := s.CreateDevice(ctx, "macos-desktop", "aa:bb:cc:dd:ee:ff", false, Schedule{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "macos-desktop" || len(plain) < 40 {
		t.Fatalf("bad device or token: %+v %q", d, plain)
	}
	if subtle.ConstantTimeCompare(HashToken(plain), d.TokenHash) != 1 {
		t.Fatal("stored hash does not match token")
	}
	if _, _, err := s.CreateDevice(ctx, "macos-desktop", "", false, Schedule{}); err != ErrExists {
		t.Fatalf("duplicate name: want ErrExists, got %v", err)
	}
}

func TestGetDeviceNotFound(t *testing.T) {
	s := openTest(t)
	if _, err := s.GetDevice(context.Background(), "nope"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRecordHandshakeReplacesCommands(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _, _ := s.CreateDevice(ctx, "deb", "", false, Schedule{})
	cmds := []proto.CommandDef{{Name: "a", TimeoutS: 5}, {Name: "b", TimeoutS: 6, ExpectDisconnect: true}}
	if err := s.RecordHandshake(ctx, d.ID, "linux", "amd64", "0.1.0", 1, []string{"metrics"}, cmds); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHandshake(ctx, d.ID, "linux", "amd64", "0.1.0", 1, []string{"metrics"}, cmds[1:]); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListCommands(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "b" || !got[0].ExpectDisconnect {
		t.Fatalf("commands not replaced: %+v", got)
	}
	d2, _ := s.GetDevice(ctx, "deb")
	if d2.OS != "linux" || d2.AgentVersion != "0.1.0" || d2.ProtocolVersion != 1 || d2.LastSeen == nil || d2.LastRejectReason != "" {
		t.Fatalf("handshake not recorded: %+v", d2)
	}
}

func TestRevokeClearsHash(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	s.CreateDevice(ctx, "x", "", false, Schedule{})
	if err := s.RevokeToken(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDevice(ctx, "x")
	if d.TokenHash != nil {
		t.Fatal("token hash not cleared")
	}
}

func TestScheduleRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	sched := Schedule{ExpectedOffline: []Window{{Days: "*", From: "01:00", To: "05:00"}}, GracePeriodS: 300}
	s.CreateDevice(ctx, "deb", "", false, sched)
	if err := s.UpdateDeviceSettings(ctx, "deb", "11:22:33:44:55:66", true, sched); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDevice(ctx, "deb")
	if !d.NormallyOff || d.MAC != "11:22:33:44:55:66" || len(d.Schedule.ExpectedOffline) != 1 || d.Schedule.GracePeriodS != 300 {
		t.Fatalf("settings not stored: %+v", d)
	}
}

func TestUsersAndSessions(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.UpsertUser(ctx, "admin", "hash1"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertUser(ctx, "admin", "hash2"); err != nil {
		t.Fatal(err)
	}
	id, hash, err := s.GetUser(ctx, "admin")
	if err != nil || hash != "hash2" {
		t.Fatalf("user: id=%d hash=%q err=%v", id, hash, err)
	}
	exp := time.Now().Add(time.Hour)
	if err := s.CreateSession(ctx, "sess1", id, exp); err != nil {
		t.Fatal(err)
	}
	uid, gotExp, err := s.GetSession(ctx, "sess1")
	if err != nil || uid != id || gotExp.Sub(exp) > time.Second {
		t.Fatalf("session: uid=%d exp=%v err=%v", uid, gotExp, err)
	}
	s.CreateSession(ctx, "old", id, time.Now().Add(-time.Hour))
	if err := s.DeleteExpiredSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.GetSession(ctx, "old"); err != ErrNotFound {
		t.Fatalf("expired session still present: %v", err)
	}
	if err := s.DeleteSession(ctx, "sess1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.GetSession(ctx, "sess1"); err != ErrNotFound {
		t.Fatal("session not deleted")
	}
}
```

**Step 2: Run to verify failure**

Run: `go test ./server/store/`
Expected: FAIL, undefined: Schedule, CreateDevice, etc.

**Step 3: Implement**

`server/store/devices.go`:

```go
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
)

// Window is one expected-offline period. Days is "*" or a comma list of
// three-letter day names ("mon,tue"). From and To are "HH:MM" server local time.
type Window struct {
	Days string `json:"days"`
	From string `json:"from"`
	To   string `json:"to"`
}

// Schedule is a device's expected downtime configuration.
type Schedule struct {
	ExpectedOffline []Window `json:"expected_offline"`
	GracePeriodS    int      `json:"grace_period_s"`
}

// Device is one row of the devices table.
type Device struct {
	ID                  int64
	Name                string
	MAC                 string
	OS                  string
	Arch                string
	TokenHash           []byte
	Capabilities        []string
	Schedule            Schedule
	NormallyOff         bool
	TerminalEnabled     bool
	LastSeen            *time.Time
	LastDisconnectAt    *time.Time
	LastRejectReason    string
	AgentVersion        string
	DesiredAgentVersion string
	ProtocolVersion     int
	CreatedAt           time.Time
}

// GenerateToken returns a new 32-byte CSPRNG token and its SHA-256 hash.
func GenerateToken() (plain string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	plain = base64.StdEncoding.EncodeToString(b)
	return plain, HashToken(plain), nil
}

// HashToken hashes a plaintext token for storage or comparison.
func HashToken(plain string) []byte {
	h := sha256.Sum256([]byte(plain))
	return h[:]
}

const deviceCols = `id, name, mac, os, arch, token_hash, capabilities_json, schedule_json,
	normally_off, terminal_enabled, last_seen, last_disconnect_at, last_reject_reason,
	agent_version, desired_agent_version, protocol_version, created_at`

type scanner interface{ Scan(dest ...any) error }

func scanDevice(row scanner) (*Device, error) {
	var d Device
	var caps, sched, created string
	var lastSeen, lastDisc sql.NullString
	var normallyOff, terminalEnabled int
	err := row.Scan(&d.ID, &d.Name, &d.MAC, &d.OS, &d.Arch, &d.TokenHash, &caps, &sched,
		&normallyOff, &terminalEnabled, &lastSeen, &lastDisc, &d.LastRejectReason,
		&d.AgentVersion, &d.DesiredAgentVersion, &d.ProtocolVersion, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(caps), &d.Capabilities)
	_ = json.Unmarshal([]byte(sched), &d.Schedule)
	if d.Capabilities == nil {
		d.Capabilities = []string{}
	}
	d.NormallyOff = normallyOff == 1
	d.TerminalEnabled = terminalEnabled == 1
	d.LastSeen = parseTime(lastSeen)
	d.LastDisconnectAt = parseTime(lastDisc)
	d.CreatedAt = mustTime(created)
	return &d, nil
}

// CreateDevice registers a device and returns the plaintext token exactly once.
func (s *Store) CreateDevice(ctx context.Context, name, mac string, normallyOff bool, sched Schedule) (*Device, string, error) {
	plain, hash, err := GenerateToken()
	if err != nil {
		return nil, "", err
	}
	schedJSON, _ := json.Marshal(sched)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO devices (name, mac, token_hash, schedule_json, normally_off, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		name, mac, hash, string(schedJSON), b2i(normallyOff), nowString())
	if isUniqueViolation(err) {
		return nil, "", ErrExists
	}
	if err != nil {
		return nil, "", err
	}
	d, err := s.GetDevice(ctx, name)
	if err != nil {
		return nil, "", err
	}
	return d, plain, nil
}

// GetDevice looks a device up by name.
func (s *Store) GetDevice(ctx context.Context, name string) (*Device, error) {
	return scanDevice(s.db.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE name = ?`, name))
}

// GetDeviceByID looks a device up by id.
func (s *Store) GetDeviceByID(ctx context.Context, id int64) (*Device, error) {
	return scanDevice(s.db.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE id = ?`, id))
}

// ListDevices returns all devices ordered by name.
func (s *Store) ListDevices(ctx context.Context) ([]*Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deviceCols+` FROM devices ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateDeviceSettings changes the operator-editable fields.
func (s *Store) UpdateDeviceSettings(ctx context.Context, name, mac string, normallyOff bool, sched Schedule) error {
	schedJSON, _ := json.Marshal(sched)
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET mac = ?, normally_off = ?, schedule_json = ? WHERE name = ?`,
		mac, b2i(normallyOff), string(schedJSON), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeToken clears the token hash so the device can no longer authenticate.
func (s *Store) RevokeToken(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET token_hash = NULL WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordHandshake stores what the agent declared and refreshes its command list.
func (s *Store) RecordHandshake(ctx context.Context, deviceID int64, osName, arch, agentVersion string, protocolVersion int, caps []string, cmds []proto.CommandDef) error {
	if caps == nil {
		caps = []string{}
	}
	capsJSON, _ := json.Marshal(caps)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := nowString()
	if _, err := tx.ExecContext(ctx,
		`UPDATE devices SET os = ?, arch = ?, agent_version = ?, protocol_version = ?, capabilities_json = ?,
		 last_seen = ?, last_reject_reason = '' WHERE id = ?`,
		osName, arch, agentVersion, protocolVersion, string(capsJSON), now, deviceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM commands WHERE device_id = ?`, deviceID); err != nil {
		return err
	}
	for _, c := range cmds {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO commands (device_id, name, description, timeout_s, expect_disconnect, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			deviceID, c.Name, c.Description, c.TimeoutS, b2i(c.ExpectDisconnect), now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListCommands returns the commands the device declared at its last handshake.
func (s *Store) ListCommands(ctx context.Context, deviceID int64) ([]proto.CommandDef, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, description, timeout_s, expect_disconnect FROM commands WHERE device_id = ? ORDER BY name`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []proto.CommandDef{}
	for rows.Next() {
		var c proto.CommandDef
		var ed int
		if err := rows.Scan(&c.Name, &c.Description, &c.TimeoutS, &ed); err != nil {
			return nil, err
		}
		c.ExpectDisconnect = ed == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

// RecordReject stores why the last handshake was refused.
func (s *Store) RecordReject(ctx context.Context, name, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE devices SET last_reject_reason = ? WHERE name = ?`, reason, name)
	return err
}

// RecordDisconnect stamps last_disconnect_at.
func (s *Store) RecordDisconnect(ctx context.Context, deviceID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE devices SET last_disconnect_at = ? WHERE id = ?`, nowString(), deviceID)
	return err
}

// TouchLastSeen stamps last_seen.
func (s *Store) TouchLastSeen(ctx context.Context, deviceID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE devices SET last_seen = ? WHERE id = ?`, nowString(), deviceID)
	return err
}
```

`server/store/users.go`:

```go
package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// UpsertUser creates the operator account or replaces its password hash.
func (s *Store) UpsertUser(ctx context.Context, username, passwordHash string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(username) DO UPDATE SET password_hash = excluded.password_hash`,
		username, passwordHash, nowString())
	return err
}

// GetUser returns the user's id and password hash.
func (s *Store) GetUser(ctx context.Context, username string) (int64, string, error) {
	var id int64
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT id, password_hash FROM users WHERE username = ?`, username).Scan(&id, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	return id, hash, err
}

// CreateSession stores a login session.
func (s *Store) CreateSession(ctx context.Context, id string, userID int64, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id, user_id, expires_at) VALUES (?, ?, ?)`,
		id, userID, timeString(expires))
	return err
}

// GetSession returns the session's user and expiry.
func (s *Store) GetSession(ctx context.Context, id string) (int64, time.Time, error) {
	var userID int64
	var exp string
	err := s.db.QueryRowContext(ctx, `SELECT user_id, expires_at FROM sessions WHERE id = ?`, id).Scan(&userID, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, time.Time{}, ErrNotFound
	}
	if err != nil {
		return 0, time.Time{}, err
	}
	return userID, mustTime(exp), nil
}

// DeleteSession logs a session out.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// DeleteExpiredSessions removes sessions past their expiry.
func (s *Store) DeleteExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, nowString())
	return err
}
```

**Step 4: Run tests**

Run: `go test ./server/store/`
Expected: PASS

**Step 5: Commit**

```bash
git add server/store/
git commit -m "feat(store): devices, tokens, commands, users, sessions"
```

---

### Task 7: `server/store` command runs

**Files:**
- Create: `server/store/runs.go`
- Test: `server/store/runs_test.go`

**Step 1: Write the failing tests**

`server/store/runs_test.go`:

```go
package store

import (
	"context"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
)

func TestRunLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _, _ := s.CreateDevice(ctx, "deb", "", false, Schedule{})
	r := &Run{ID: "run1", DeviceID: d.ID, Command: "uptime", RequestedBy: "admin", RequestedAt: time.Now(), Status: proto.RunRunning}
	if err := s.InsertRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	code := 0
	if err := s.FinishRun(ctx, "run1", proto.RunOK, &code, "out", "", false); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRun(ctx, "run1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != proto.RunOK || got.ExitCode == nil || *got.ExitCode != 0 || got.StdoutTail != "out" || got.FinishedAt == nil || got.DeviceName != "deb" {
		t.Fatalf("finish not recorded: %+v", got)
	}
}

func TestMarkLost(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d1, _, _ := s.CreateDevice(ctx, "a", "", false, Schedule{})
	d2, _, _ := s.CreateDevice(ctx, "b", "", false, Schedule{})
	now := time.Now()
	s.InsertRun(ctx, &Run{ID: "r1", DeviceID: d1.ID, Command: "x", RequestedBy: "u", RequestedAt: now, Status: proto.RunRunning})
	s.InsertRun(ctx, &Run{ID: "r2", DeviceID: d1.ID, Command: "x", RequestedBy: "u", RequestedAt: now, Status: proto.RunOK})
	s.InsertRun(ctx, &Run{ID: "r3", DeviceID: d2.ID, Command: "x", RequestedBy: "u", RequestedAt: now, Status: proto.RunDispatched})

	n, err := s.MarkDeviceRunsLost(ctx, d1.ID)
	if err != nil || n != 1 {
		t.Fatalf("MarkDeviceRunsLost n=%d err=%v", n, err)
	}
	r1, _ := s.GetRun(ctx, "r1")
	r3, _ := s.GetRun(ctx, "r3")
	if r1.Status != proto.RunLost || r3.Status != proto.RunDispatched {
		t.Fatalf("wrong rows marked: r1=%s r3=%s", r1.Status, r3.Status)
	}
	n, err = s.MarkAllInFlightLost(ctx)
	if err != nil || n != 1 {
		t.Fatalf("MarkAllInFlightLost n=%d err=%v", n, err)
	}
	r3, _ = s.GetRun(ctx, "r3")
	if r3.Status != proto.RunLost {
		t.Fatal("dispatched run not marked lost at startup")
	}
}

func TestListRunsNewestFirst(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _, _ := s.CreateDevice(ctx, "a", "", false, Schedule{})
	base := time.Now()
	for i := 0; i < 3; i++ {
		s.InsertRun(ctx, &Run{ID: string(rune('a' + i)), DeviceID: d.ID, Command: "x", RequestedBy: "u", RequestedAt: base.Add(time.Duration(i) * time.Second), Status: proto.RunOK})
	}
	runs, err := s.ListRuns(ctx, d.ID, 2)
	if err != nil || len(runs) != 2 || runs[0].ID != "c" {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
}
```

**Step 2: Run to verify failure**

Run: `go test ./server/store/`
Expected: FAIL, undefined: Run

**Step 3: Implement**

`server/store/runs.go`:

```go
package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
)

// Run is one command_runs row.
type Run struct {
	ID          string     `json:"id"`
	DeviceID    int64      `json:"device_id"`
	DeviceName  string     `json:"device"`
	Command     string     `json:"command"`
	RequestedBy string     `json:"requested_by"`
	RequestedAt time.Time  `json:"requested_at"`
	StartedAt   *time.Time `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at"`
	ExitCode    *int       `json:"exit_code"`
	Status      string     `json:"status"`
	StdoutTail  string     `json:"stdout_tail"`
	StderrTail  string     `json:"stderr_tail"`
	Truncated   bool       `json:"truncated"`
}

const runCols = `r.id, r.device_id, d.name, r.command, r.requested_by, r.requested_at, r.started_at,
	r.finished_at, r.exit_code, r.status, r.stdout_tail, r.stderr_tail, r.truncated`

func scanRun(row scanner) (*Run, error) {
	var r Run
	var reqAt string
	var started, finished sql.NullString
	var exit sql.NullInt64
	var trunc int
	err := row.Scan(&r.ID, &r.DeviceID, &r.DeviceName, &r.Command, &r.RequestedBy, &reqAt, &started,
		&finished, &exit, &r.Status, &r.StdoutTail, &r.StderrTail, &trunc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.RequestedAt = mustTime(reqAt)
	r.StartedAt = parseTime(started)
	r.FinishedAt = parseTime(finished)
	if exit.Valid {
		v := int(exit.Int64)
		r.ExitCode = &v
	}
	r.Truncated = trunc == 1
	return &r, nil
}

// InsertRun records a new run. StartedAt is set to RequestedAt.
func (s *Store) InsertRun(ctx context.Context, r *Run) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO command_runs (id, device_id, command, requested_by, requested_at, started_at, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.DeviceID, r.Command, r.RequestedBy, timeString(r.RequestedAt), timeString(r.RequestedAt), r.Status)
	return err
}

// SetRunStatus changes status only (used for dispatched).
func (s *Store) SetRunStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE command_runs SET status = ? WHERE id = ?`, status, id)
	return err
}

// FinishRun records a terminal status and output.
func (s *Store) FinishRun(ctx context.Context, id, status string, exitCode *int, stdout, stderr string, truncated bool) error {
	var exit any
	if exitCode != nil {
		exit = *exitCode
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE command_runs SET status = ?, exit_code = ?, stdout_tail = ?, stderr_tail = ?, truncated = ?, finished_at = ?
		 WHERE id = ?`,
		status, exit, stdout, stderr, b2i(truncated), nowString(), id)
	return err
}

// MarkDeviceRunsLost sets every running row for a device to lost.
// Dispatched rows are handled by the router, which decides between
// disconnected_as_expected and lost.
func (s *Store) MarkDeviceRunsLost(ctx context.Context, deviceID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE command_runs SET status = ?, finished_at = ? WHERE device_id = ? AND status = ?`,
		proto.RunLost, nowString(), deviceID, proto.RunRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// MarkAllInFlightLost is run once at server startup.
func (s *Store) MarkAllInFlightLost(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE command_runs SET status = ?, finished_at = ? WHERE status IN (?, ?)`,
		proto.RunLost, nowString(), proto.RunRunning, proto.RunDispatched)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetRun fetches one run.
func (s *Store) GetRun(ctx context.Context, id string) (*Run, error) {
	return scanRun(s.db.QueryRowContext(ctx,
		`SELECT `+runCols+` FROM command_runs r JOIN devices d ON d.id = r.device_id WHERE r.id = ?`, id))
}

// ListRuns returns the newest runs for a device.
func (s *Store) ListRuns(ctx context.Context, deviceID int64, limit int) ([]*Run, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runCols+` FROM command_runs r JOIN devices d ON d.id = r.device_id
		 WHERE r.device_id = ? ORDER BY r.requested_at DESC LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
```

**Step 4: Run tests**

Run: `go test ./server/store/`
Expected: PASS

**Step 5: Commit**

```bash
git add server/store/
git commit -m "feat(store): command runs with lost marking"
```

---

### Task 8: Server config and `kumaboard serve` skeleton

**Files:**
- Create: `server/config/config.go`
- Test: `server/config/config_test.go`
- Create: `cmd/kumaboard/main.go`
- Create: `cmd/kumaboard/serve.go`
- Create: `configs/kumaboard.example.yaml`

**Step 1: Write the failing test**

`server/config/config_test.go`:

```go
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
```

**Step 2: Run to verify failure**

Run: `go test ./server/config/`
Expected: FAIL, undefined: Load

**Step 3: Implement**

`server/config/config.go`:

```go
// Package config loads the server configuration file.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"

	"gopkg.in/yaml.v3"
)

// WOL is the magic packet sender configuration.
type WOL struct {
	Interface string `yaml:"interface"`
	Broadcast string `yaml:"broadcast"`
}

// Config is /etc/kumaboard/config.yaml.
type Config struct {
	ListenAddrs        []string `yaml:"listen_addrs"`
	Hostnames          []string `yaml:"hostnames"`
	DataDir            string   `yaml:"data_dir"`
	WOL                WOL      `yaml:"wol"`
	MetricsIntervalS   int      `yaml:"metrics_interval_s"`
	HeartbeatIntervalS int      `yaml:"heartbeat_interval_s"`
}

// Load reads, defaults, and validates the config.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		DataDir:            "/var/lib/kumaboard",
		MetricsIntervalS:   30,
		HeartbeatIntervalS: 15,
	}
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if len(cfg.ListenAddrs) == 0 {
		return nil, errors.New("config: listen_addrs must list at least one host:port")
	}
	for _, a := range cfg.ListenAddrs {
		host, _, err := net.SplitHostPort(a)
		if err != nil {
			return nil, fmt.Errorf("config: listen_addrs %q: %w", a, err)
		}
		ip := net.ParseIP(host)
		if host == "" || (ip != nil && ip.IsUnspecified()) {
			return nil, fmt.Errorf("config: listen_addrs %q binds all interfaces; use a specific IP", a)
		}
	}
	if cfg.MetricsIntervalS < 5 || cfg.HeartbeatIntervalS < 5 {
		return nil, errors.New("config: intervals must be at least 5 seconds")
	}
	return cfg, nil
}

// SANs are the certificate subject alternative names: listen hosts plus hostnames.
func (c *Config) SANs() []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range c.ListenAddrs {
		host, _, _ := net.SplitHostPort(a)
		if !seen[host] {
			seen[host] = true
			out = append(out, host)
		}
	}
	for _, h := range c.Hostnames {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// AllowedHosts is the set of acceptable Host header values.
func (c *Config) AllowedHosts() map[string]bool {
	out := map[string]bool{}
	ports := map[string]bool{}
	for _, a := range c.ListenAddrs {
		out[a] = true
		_, port, _ := net.SplitHostPort(a)
		ports[port] = true
	}
	for _, h := range c.Hostnames {
		out[h] = true
		for p := range ports {
			out[net.JoinHostPort(h, p)] = true
		}
	}
	return out
}
```

`cmd/kumaboard/main.go`:

```go
// Command kumaboard is the control plane server.
package main

import (
	"fmt"
	"os"

	"github.com/jhyoong/KumaBoard/internal/buildinfo"
	"github.com/jhyoong/KumaBoard/proto"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage: kumaboard <command> [flags]

commands:
  serve          run the server (flags: -config PATH)
  passwd         create the operator account or reset its password (flags: -config PATH)
  cert reissue   issue a new server certificate from the existing CA (flags: -config PATH)
  version        print version and protocol version`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(os.Args[2:])
	case "passwd":
		err = runPasswd(os.Args[2:])
	case "cert":
		if len(os.Args) < 3 || os.Args[2] != "reissue" {
			usage()
		}
		err = runCertReissue(os.Args[3:])
	case "version":
		fmt.Printf("kumaboard %s protocol %d\n", buildinfo.Version, proto.Version)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
```

`cmd/kumaboard/serve.go` (skeleton; later tasks add the hub, registry, API, and web handler where marked):

```go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jhyoong/KumaBoard/internal/buildinfo"
	"github.com/jhyoong/KumaBoard/server/config"
	"github.com/jhyoong/KumaBoard/server/pki"
	"github.com/jhyoong/KumaBoard/server/store"
)

const defaultConfigPath = "/etc/kumaboard/config.yaml"

func loadConfigFlag(args []string) (*config.Config, error) {
	fs := flag.NewFlagSet("kumaboard", flag.ContinueOnError)
	path := fs.String("config", defaultConfigPath, "path to config.yaml")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return config.Load(*path)
}

func runServe(args []string) error {
	cfg, err := loadConfigFlag(args)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	pkiDir := filepath.Join(cfg.DataDir, "pki")
	if err := pki.Ensure(pkiDir, cfg.SANs()); err != nil {
		return err
	}
	if exp, err := pki.ServerCertExpiry(pkiDir); err == nil && time.Until(exp) < 60*24*time.Hour {
		log.Warn("server certificate expires soon; run: kumaboard cert reissue", "expires", exp)
	}
	tlsCfg, err := pki.TLSConfig(pkiDir)
	if err != nil {
		return err
	}

	st, err := store.Open(filepath.Join(cfg.DataDir, "kumaboard.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	if n, err := st.MarkAllInFlightLost(context.Background()); err != nil {
		return err
	} else if n > 0 {
		log.Info("marked in-flight runs lost at startup", "count", n)
	}

	handler, err := buildHandler(cfg, st, log)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler:           handler,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, len(cfg.ListenAddrs))
	for _, addr := range cfg.ListenAddrs {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", addr, err)
		}
		log.Info("listening", "addr", addr, "version", buildinfo.Version)
		go func() { errCh <- srv.ServeTLS(ln, "", "") }()
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-stop:
		log.Info("shutting down", "signal", sig)
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

// buildHandler wires the HTTP handler. Later tasks add the hub, registry,
// SSE broker, API, and embedded frontend here.
func buildHandler(cfg *config.Config, st *store.Store, log *slog.Logger) (http.Handler, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	return mux, nil
}

func runPasswd(args []string) error {
	return errors.New("passwd: not implemented yet")
}

func runCertReissue(args []string) error {
	cfg, err := loadConfigFlag(args)
	if err != nil {
		return err
	}
	dir := filepath.Join(cfg.DataDir, "pki")
	if err := pki.Reissue(dir, cfg.SANs()); err != nil {
		return err
	}
	fmt.Println("server certificate reissued; restart kumaboard to load it")
	return nil
}
```

`configs/kumaboard.example.yaml`:

```yaml
# /etc/kumaboard/config.yaml
listen_addrs: ["192.168.1.150:8443"]
hostnames: []
data_dir: /var/lib/kumaboard
wol:
  interface: eth0
  broadcast: 192.168.1.255
metrics_interval_s: 30
heartbeat_interval_s: 15
```

**Step 4: Fetch yaml, run tests, and smoke-test serve**

```bash
go get gopkg.in/yaml.v3@latest
go test ./server/config/
go build ./cmd/kumaboard
```

Then run the server locally against a scratch directory. Find the macOS desktop's LAN IP first:

```bash
ipconfig getifaddr en0 || ipconfig getifaddr en1
```

Create `/tmp/kb/config.yaml` with `listen_addrs: ["127.0.0.1:8443"]` and `data_dir: /tmp/kb/data`, then:

```bash
./kumaboard serve -config /tmp/kb/config.yaml &
sleep 1
curl --cacert /tmp/kb/data/pki/ca.pem https://127.0.0.1:8443/healthz
kill %1
./kumaboard version
```

Expected: `ok`, then `kumaboard dev protocol 1`. Verify `/tmp/kb/data/pki/` contains `ca.pem`, `ca.key`, `server.pem`, `server.key`.

**Step 5: Commit**

```bash
git add server/config/ cmd/kumaboard/ configs/ go.mod go.sum
git commit -m "feat(server): config loading and serve skeleton with TLS listeners"
```

---

## Build step 3: Hub, agent transport, first connection

### Task 9: `server/hub` handshake, sessions, liveness

**Files:**
- Create: `server/hub/hub.go`
- Create: `server/hub/session.go`
- Test: `server/hub/hub_test.go`

**Step 1: Write the failing tests**

`server/hub/hub_test.go`:

```go
package hub

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, s)
}
func (r *recorder) DeviceConnected(name string)                   { r.add("connect:" + name) }
func (r *recorder) DeviceDisconnected(name string)                { r.add("disconnect:" + name) }
func (r *recorder) MetricsReceived(name string, m proto.Metrics)  { r.add("metrics:" + name) }
func (r *recorder) RunChanged(runID string)                       { r.add("run:" + runID) }
func (r *recorder) has(s string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e == s {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func newTestHub(t *testing.T, opts Options) (*Hub, *store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	opts.Store = st
	if opts.Events == nil {
		opts.Events = &recorder{}
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if opts.MetricsInterval == 0 {
		opts.MetricsInterval = 30 * time.Second
	}
	if opts.HeartbeatInterval == 0 {
		opts.HeartbeatInterval = 15 * time.Second
	}
	h := New(opts)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return h, st, "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dialHello(t *testing.T, url string, hello proto.Hello) (*websocket.Conn, *proto.Envelope) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := proto.New(proto.TypeHello, hello)
	b, _ := proto.Encode(env)
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := proto.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	return conn, resp
}

func goodHello(name, token string) proto.Hello {
	return proto.Hello{ProtocolVersion: proto.Version, AgentVersion: "0.1.0", DeviceName: name,
		Token: token, OS: "linux", Arch: "amd64", Capabilities: []string{"metrics"}}
}

func errCode(t *testing.T, env *proto.Envelope) string {
	t.Helper()
	if env.Type != proto.TypeError {
		t.Fatalf("want error, got %s", env.Type)
	}
	var e proto.Error
	env.Unmarshal(&e)
	return e.Code
}

func TestHandshakeOK(t *testing.T) {
	rec := &recorder{}
	h, st, url := newTestHub(t, Options{Events: rec})
	_, token, _ := st.CreateDevice(context.Background(), "dev", "", false, store.Schedule{})
	conn, resp := dialHello(t, url, goodHello("dev", token))
	defer conn.CloseNow()
	if resp.Type != proto.TypeHelloAck {
		t.Fatalf("want hello_ack, got %s", resp.Type)
	}
	var ack proto.HelloAck
	resp.Unmarshal(&ack)
	if ack.SessionID == "" || ack.MetricsIntervalS != 30 || ack.HeartbeatIntervalS != 15 {
		t.Fatalf("bad ack: %+v", ack)
	}
	waitFor(t, func() bool { return rec.has("connect:dev") && h.Connected("dev") })
	d, _ := st.GetDevice(context.Background(), "dev")
	if d.AgentVersion != "0.1.0" || d.OS != "linux" {
		t.Fatalf("handshake not recorded: %+v", d)
	}
}

func TestHandshakeErrors(t *testing.T) {
	_, st, url := newTestHub(t, Options{})
	_, token, _ := st.CreateDevice(context.Background(), "dev", "", false, store.Schedule{})

	_, resp := dialHello(t, url, goodHello("nope", token))
	if c := errCode(t, resp); c != proto.ErrUnknownDevice {
		t.Fatalf("got %s", c)
	}
	_, resp = dialHello(t, url, goodHello("dev", "wrong"))
	if c := errCode(t, resp); c != proto.ErrAuthFailed {
		t.Fatalf("got %s", c)
	}
	bad := goodHello("dev", token)
	bad.ProtocolVersion = 99
	_, resp = dialHello(t, url, bad)
	if c := errCode(t, resp); c != proto.ErrProtocolVersionUnsupported {
		t.Fatalf("got %s", c)
	}
	d, _ := st.GetDevice(context.Background(), "dev")
	if d.LastRejectReason != proto.ErrProtocolVersionUnsupported {
		t.Fatalf("reject reason not recorded: %q", d.LastRejectReason)
	}
	st.RevokeToken(context.Background(), "dev")
	_, resp = dialHello(t, url, goodHello("dev", token))
	if c := errCode(t, resp); c != proto.ErrAuthFailed {
		t.Fatalf("revoked token accepted: %s", c)
	}
}

func TestDuplicateSessionReplacesOld(t *testing.T) {
	rec := &recorder{}
	h, st, url := newTestHub(t, Options{Events: rec})
	_, token, _ := st.CreateDevice(context.Background(), "dev", "", false, store.Schedule{})
	c1, _ := dialHello(t, url, goodHello("dev", token))
	c2, resp := dialHello(t, url, goodHello("dev", token))
	defer c2.CloseNow()
	if resp.Type != proto.TypeHelloAck {
		t.Fatalf("second session refused: %s", resp.Type)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := c1.Read(ctx); err == nil {
		t.Fatal("old connection still open")
	}
	waitFor(t, func() bool { return h.SessionCount() == 1 && h.Connected("dev") })
	if rec.has("disconnect:dev") {
		t.Fatal("replacement must not emit a disconnect for the device")
	}
}

func TestPongTimeoutCloses(t *testing.T) {
	_, st, url := newTestHub(t, Options{HeartbeatInterval: 50 * time.Millisecond, PongTimeout: 50 * time.Millisecond})
	_, token, _ := st.CreateDevice(context.Background(), "dev", "", false, store.Schedule{})
	conn, _ := dialHello(t, url, goodHello("dev", token))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Read pings but never answer. The server must close us.
	for {
		_, _, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				t.Fatal("server did not close a client that never pongs")
			}
			return
		}
	}
}

func TestPongKeepsAlive(t *testing.T) {
	h, st, url := newTestHub(t, Options{HeartbeatInterval: 30 * time.Millisecond, PongTimeout: 100 * time.Millisecond})
	_, token, _ := st.CreateDevice(context.Background(), "dev", "", false, store.Schedule{})
	conn, _ := dialHello(t, url, goodHello("dev", token))
	defer conn.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	for ctx.Err() == nil {
		_, data, err := conn.Read(ctx)
		if err != nil {
			break
		}
		env, _ := proto.Decode(data)
		if env.Type == proto.TypePing {
			pong, _ := proto.Reply(env, proto.TypePong, nil)
			b, _ := proto.Encode(pong)
			conn.Write(context.Background(), websocket.MessageText, b)
		}
	}
	if !h.Connected("dev") {
		t.Fatal("session dropped despite pongs")
	}
}

func TestOversizeMessageCloses(t *testing.T) {
	_, st, url := newTestHub(t, Options{})
	_, token, _ := st.CreateDevice(context.Background(), "dev", "", false, store.Schedule{})
	conn, _ := dialHello(t, url, goodHello("dev", token))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	big := []byte(`{"v":1,"id":"x","type":"metrics","payload":"` + strings.Repeat("a", proto.MaxMessageSize) + `"}`)
	conn.Write(ctx, websocket.MessageText, big)
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("server accepted an oversized frame")
	}
}
```

**Step 2: Run to verify failure**

Run: `go get github.com/coder/websocket@latest && go test ./server/hub/`
Expected: FAIL, undefined: Options, New

**Step 3: Implement**

`server/hub/hub.go`:

```go
// Package hub owns agent control-socket sessions.
package hub

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/oklog/ulid/v2"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

// Events receives notifications about sessions. The registry implements it.
type Events interface {
	DeviceConnected(name string)
	DeviceDisconnected(name string)
	MetricsReceived(name string, m proto.Metrics)
	RunChanged(runID string)
}

// Options configures a Hub.
type Options struct {
	Store             *store.Store
	Events            Events
	Log               *slog.Logger
	MetricsInterval   time.Duration
	HeartbeatInterval time.Duration
	PongTimeout       time.Duration // default 10s
}

// Hub accepts agent connections at /ws and tracks one session per device.
type Hub struct {
	opts     Options
	mu       sync.Mutex
	sessions map[string]*Session // by device name
}

// New creates a Hub.
func New(opts Options) *Hub {
	if opts.PongTimeout == 0 {
		opts.PongTimeout = 10 * time.Second
	}
	return &Hub{opts: opts, sessions: map[string]*Session{}}
}

// ServeHTTP upgrades the request and runs the session until it ends.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		h.opts.Log.Warn("ws accept failed", "remote", r.RemoteAddr, "err", err)
		return
	}
	conn.SetReadLimit(proto.MaxMessageSize)
	h.serve(r.Context(), conn, r.RemoteAddr)
}

func (h *Hub) serve(ctx context.Context, conn *websocket.Conn, remote string) {
	defer conn.CloseNow()
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	env, err := readEnvelope(hctx, conn)
	cancel()
	if err != nil || env.Type != proto.TypeHello {
		conn.Close(websocket.StatusProtocolError, "expected hello")
		return
	}
	var hello proto.Hello
	if err := env.Unmarshal(&hello); err != nil {
		conn.Close(websocket.StatusProtocolError, "bad hello")
		return
	}
	sess, code := h.authenticate(ctx, &hello, remote)
	if code != "" {
		e, _ := proto.Reply(env, proto.TypeError, proto.Error{Code: code, Message: "handshake rejected"})
		writeEnvelope(ctx, conn, e)
		conn.Close(websocket.StatusPolicyViolation, code)
		return
	}
	sess.conn = conn
	h.register(ctx, sess)
	defer h.unregister(ctx, sess)

	ack := proto.HelloAck{
		SessionID:          sess.ID,
		ServerTime:         time.Now().UTC(),
		MetricsIntervalS:   int(h.opts.MetricsInterval / time.Second),
		HeartbeatIntervalS: int(h.opts.HeartbeatInterval / time.Second),
	}
	ackEnv, _ := proto.Reply(env, proto.TypeHelloAck, ack)
	if err := sess.Send(ackEnv); err != nil {
		return
	}
	h.opts.Events.DeviceConnected(sess.DeviceName)
	h.opts.Log.Info("agent connected", "device", sess.DeviceName, "version", hello.AgentVersion, "remote", remote)

	go sess.pinger(ctx, h.opts.HeartbeatInterval, h.opts.PongTimeout)
	sess.readLoop(ctx, h)
}

// authenticate checks the device, token, then protocol version, in that order,
// so only an authenticated agent can record a reject reason on its own row.
func (h *Hub) authenticate(ctx context.Context, hello *proto.Hello, remote string) (*Session, string) {
	st := h.opts.Store
	d, err := st.GetDevice(ctx, hello.DeviceName)
	if err != nil {
		st.Audit(ctx, "agent:"+hello.DeviceName, "handshake", hello.DeviceName, proto.ErrUnknownDevice, remote)
		return nil, proto.ErrUnknownDevice
	}
	if d.TokenHash == nil || subtle.ConstantTimeCompare(store.HashToken(hello.Token), d.TokenHash) != 1 {
		st.RecordReject(ctx, d.Name, proto.ErrAuthFailed)
		st.Audit(ctx, "agent:"+d.Name, "handshake", d.Name, proto.ErrAuthFailed, remote)
		return nil, proto.ErrAuthFailed
	}
	if !proto.Supported(hello.ProtocolVersion) {
		st.RecordReject(ctx, d.Name, proto.ErrProtocolVersionUnsupported)
		st.Audit(ctx, "agent:"+d.Name, "handshake", d.Name, proto.ErrProtocolVersionUnsupported,
			"agent protocol "+strconv.Itoa(hello.ProtocolVersion)+" version "+hello.AgentVersion)
		return nil, proto.ErrProtocolVersionUnsupported
	}
	if err := st.RecordHandshake(ctx, d.ID, hello.OS, hello.Arch, hello.AgentVersion, hello.ProtocolVersion, hello.Capabilities, hello.Commands); err != nil {
		h.opts.Log.Error("record handshake", "device", d.Name, "err", err)
		return nil, proto.ErrProtocol
	}
	st.Audit(ctx, "agent:"+d.Name, "handshake", d.Name, "ok", "version "+hello.AgentVersion)
	return &Session{
		ID:         ulid.Make().String(),
		DeviceID:   d.ID,
		DeviceName: d.Name,
		closed:     make(chan struct{}),
		log:        h.opts.Log,
	}, ""
}

func (h *Hub) register(ctx context.Context, s *Session) {
	h.mu.Lock()
	old := h.sessions[s.DeviceName]
	h.sessions[s.DeviceName] = s
	h.mu.Unlock()
	if old != nil {
		h.opts.Log.Info("replacing duplicate session", "device", s.DeviceName)
		h.opts.Store.Audit(ctx, "agent:"+s.DeviceName, "session_replaced", s.DeviceName, "ok", "")
		old.close(websocket.StatusPolicyViolation, proto.ErrDuplicateSession)
	}
}

func (h *Hub) unregister(ctx context.Context, s *Session) {
	s.close(websocket.StatusNormalClosure, "bye")
	h.mu.Lock()
	current := h.sessions[s.DeviceName] == s
	if current {
		delete(h.sessions, s.DeviceName)
	}
	h.mu.Unlock()
	h.sessionEnded(ctx, s)
	if current {
		h.opts.Store.RecordDisconnect(ctx, s.DeviceID)
		h.opts.Events.DeviceDisconnected(s.DeviceName)
		h.opts.Log.Info("agent disconnected", "device", s.DeviceName)
	}
}

// sessionEnded is extended by the command router (router.go).
func (h *Hub) sessionEnded(ctx context.Context, s *Session) {}

// handleMessage dispatches one message after the handshake.
func (h *Hub) handleMessage(ctx context.Context, s *Session, env *proto.Envelope) {
	switch env.Type {
	case proto.TypePong:
		s.lastPong.Store(time.Now().UnixNano())
	case proto.TypePing:
		pong, _ := proto.Reply(env, proto.TypePong, nil)
		s.Send(pong)
	case proto.TypeMetrics:
		var m proto.Metrics
		if err := env.Unmarshal(&m); err != nil {
			return
		}
		h.opts.Store.TouchLastSeen(ctx, s.DeviceID)
		h.opts.Events.MetricsReceived(s.DeviceName, m)
	default:
		h.opts.Log.Debug("unhandled message", "device", s.DeviceName, "type", env.Type)
	}
}

// Connected reports whether a device has a live session.
func (h *Hub) Connected(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.sessions[name]
	return ok
}

// SessionCount is the number of live sessions.
func (h *Hub) SessionCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions)
}

// ConnectedNames lists devices with live sessions.
func (h *Hub) ConnectedNames() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.sessions))
	for n := range h.sessions {
		out = append(out, n)
	}
	return out
}

// CloseDevice ends a device's session, for example after token revocation.
func (h *Hub) CloseDevice(name, reason string) {
	h.mu.Lock()
	s := h.sessions[name]
	h.mu.Unlock()
	if s != nil {
		s.close(websocket.StatusPolicyViolation, reason)
	}
}

func (h *Hub) session(name string) *Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[name]
}
```

`server/hub/session.go`:

```go
package hub

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/jhyoong/KumaBoard/proto"
)

// Session is one live agent connection.
type Session struct {
	ID         string
	DeviceID   int64
	DeviceName string

	conn      *websocket.Conn
	writeMu   sync.Mutex
	closeOnce sync.Once
	closed    chan struct{}
	lastPong  atomic.Int64
	log       *slog.Logger
}

// Send writes one envelope. Safe for concurrent use.
func (s *Session) Send(env *proto.Envelope) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return writeEnvelope(ctx, s.conn, env)
}

func (s *Session) close(code websocket.StatusCode, reason string) {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.conn.Close(code, reason)
	})
}

// Closed is closed when the session ends.
func (s *Session) Closed() <-chan struct{} { return s.closed }

func (s *Session) pinger(ctx context.Context, interval, pongTimeout time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		case <-t.C:
			sent := time.Now().UnixNano()
			env, _ := proto.New(proto.TypePing, nil)
			if err := s.Send(env); err != nil {
				s.close(websocket.StatusGoingAway, "ping write failed")
				return
			}
			time.AfterFunc(pongTimeout, func() {
				if s.lastPong.Load() < sent {
					s.log.Warn("pong timeout", "device", s.DeviceName)
					s.close(websocket.StatusGoingAway, "pong timeout")
				}
			})
		}
	}
}

func (s *Session) readLoop(ctx context.Context, h *Hub) {
	for {
		env, err := readEnvelope(ctx, s.conn)
		if err != nil {
			return
		}
		h.handleMessage(ctx, s, env)
	}
}

func readEnvelope(ctx context.Context, conn *websocket.Conn) (*proto.Envelope, error) {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	return proto.Decode(data)
}

func writeEnvelope(ctx context.Context, conn *websocket.Conn, env *proto.Envelope) error {
	b, err := proto.Encode(env)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, b)
}
```

**Step 4: Run tests**

Run: `go test ./server/hub/ -race`
Expected: PASS

**Step 5: Commit**

```bash
git add server/hub/ go.mod go.sum
git commit -m "feat(hub): agent handshake, token auth, duplicate sessions, ping/pong"
```

---

### Task 10: `agent/config`

**Files:**
- Create: `agent/config/config.go`
- Test: `agent/config/config_test.go`

**Step 1: Write the failing test**

`agent/config/config_test.go`:

```go
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
```

**Step 2: Run to verify failure**

Run: `go test ./agent/config/`
Expected: FAIL, undefined: Load

**Step 3: Implement**

`agent/config/config.go`:

```go
// Package config loads the agent's YAML config, CA file, and token file.
package config

import (
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jhyoong/KumaBoard/proto"
)

// Command is one named command. Run is an argv array, never a shell string.
type Command struct {
	Description      string   `yaml:"description"`
	Run              []string `yaml:"run"`
	TimeoutS         int      `yaml:"timeout_s"`
	ExpectDisconnect bool     `yaml:"expect_disconnect"`
}

// Config is /etc/kuma-agent/config.yaml plus the loaded secrets.
type Config struct {
	Device struct {
		Name string `yaml:"name"`
	} `yaml:"device"`
	Server struct {
		URL    string `yaml:"url"`
		CAFile string `yaml:"ca_file"`
	} `yaml:"server"`
	TokenFile    string             `yaml:"token_file"`
	Capabilities []string           `yaml:"capabilities"`
	Commands     map[string]Command `yaml:"commands"`

	// Loaded, not parsed from YAML.
	Token  string         `yaml:"-"`
	CAPool *x509.CertPool `yaml:"-"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Load reads and validates the config and its referenced files.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	if !nameRe.MatchString(cfg.Device.Name) {
		return nil, errors.New("config: device.name must be lowercase letters, digits, and hyphens")
	}
	if !strings.HasPrefix(cfg.Server.URL, "wss://") {
		return nil, errors.New("config: server.url must start with wss://")
	}
	caPEM, err := os.ReadFile(cfg.Server.CAFile)
	if err != nil {
		return nil, fmt.Errorf("config: read ca_file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("config: ca_file contains no certificate")
	}
	cfg.CAPool = pool
	tok, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("config: read token_file: %w", err)
	}
	cfg.Token = strings.TrimSpace(string(tok))
	if cfg.Token == "" {
		return nil, errors.New("config: token file is empty")
	}
	if cfg.Capabilities == nil {
		cfg.Capabilities = []string{}
	}
	for name, c := range cfg.Commands {
		if !nameRe.MatchString(name) {
			return nil, fmt.Errorf("config: command %q: bad name", name)
		}
		if len(c.Run) == 0 || c.Run[0] == "" {
			return nil, fmt.Errorf("config: command %q: run must be a non-empty argv array", name)
		}
		if c.TimeoutS <= 0 {
			return nil, fmt.Errorf("config: command %q: timeout_s must be positive", name)
		}
	}
	return &cfg, nil
}

// CommandDefs is what the agent declares in hello, sorted by name.
func (c *Config) CommandDefs() []proto.CommandDef {
	out := make([]proto.CommandDef, 0, len(c.Commands))
	for name, cmd := range c.Commands {
		out = append(out, proto.CommandDef{
			Name: name, Description: cmd.Description, TimeoutS: cmd.TimeoutS, ExpectDisconnect: cmd.ExpectDisconnect,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
```

**Step 4: Run tests**

Run: `go test ./agent/config/`
Expected: PASS

**Step 5: Commit**

```bash
git add agent/config/
git commit -m "feat(agent): config loading with pinned CA and token file"
```

---

### Task 11: `agent/transport` dial, backoff, sleep detection

**Files:**
- Create: `agent/transport/backoff.go`
- Create: `agent/transport/client.go`
- Test: `agent/transport/backoff_test.go`

**Step 1: Write the failing test**

`agent/transport/backoff_test.go`:

```go
package transport

import (
	"testing"
	"time"
)

func within(d, base time.Duration) bool {
	return d >= time.Duration(float64(base)*0.8) && d <= time.Duration(float64(base)*1.2)
}

func TestBackoffDoublesToCeiling(t *testing.T) {
	b := NewBackoff(time.Second, 60*time.Second, 0.2)
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60}
	for i, w := range want {
		got := b.Next()
		if !within(got, w*time.Second) {
			t.Fatalf("step %d: got %v, want about %v", i, got, w*time.Second)
		}
	}
	b.Reset()
	if got := b.Next(); !within(got, time.Second) {
		t.Fatalf("after reset: %v", got)
	}
	b.Saturate()
	if got := b.Next(); !within(got, 60*time.Second) {
		t.Fatalf("after saturate: %v", got)
	}
}

func TestSleepDetected(t *testing.T) {
	last := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if sleepDetected(last, last.Add(20*time.Second), 15*time.Second, 30*time.Second) {
		t.Fatal("normal tick flagged as sleep")
	}
	if !sleepDetected(last, last.Add(50*time.Second), 15*time.Second, 30*time.Second) {
		t.Fatal("50s jump not flagged")
	}
}
```

**Step 2: Run to verify failure**

Run: `go test ./agent/transport/`
Expected: FAIL, undefined: NewBackoff

**Step 3: Implement**

`agent/transport/backoff.go`:

```go
package transport

import (
	"math/rand/v2"
	"time"
)

// Backoff doubles from Min to Max with symmetric jitter.
type Backoff struct {
	min, max time.Duration
	jitter   float64
	cur      time.Duration
}

// NewBackoff creates a backoff. jitter is a fraction, e.g. 0.2 for plus or minus 20 percent.
func NewBackoff(min, max time.Duration, jitter float64) *Backoff {
	return &Backoff{min: min, max: max, jitter: jitter}
}

// Next returns the next delay and advances.
func (b *Backoff) Next() time.Duration {
	if b.cur == 0 {
		b.cur = b.min
	} else {
		b.cur *= 2
		if b.cur > b.max {
			b.cur = b.max
		}
	}
	f := 1 + (rand.Float64()*2-1)*b.jitter
	return time.Duration(float64(b.cur) * f)
}

// Reset returns to the minimum delay.
func (b *Backoff) Reset() { b.cur = 0 }

// Saturate jumps to the ceiling. Used after a handshake rejection.
func (b *Backoff) Saturate() { b.cur = b.max }

// sleepDetected reports whether wall-clock time jumped more than the tick
// interval plus the allowed slack, which means the machine was asleep.
func sleepDetected(last, now time.Time, interval, slack time.Duration) bool {
	return now.Sub(last) > interval+slack
}
```

`agent/transport/client.go`:

```go
// Package transport dials the control plane and keeps the connection alive.
package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/jhyoong/KumaBoard/proto"
)

// Sender writes envelopes to the current connection.
type Sender interface {
	Send(env *proto.Envelope) error
}

// Handler receives connection events and non-liveness messages.
type Handler interface {
	OnConnected(ack proto.HelloAck, send Sender)
	OnDisconnected()
	OnMessage(env *proto.Envelope, send Sender)
}

// Client is the agent side of the control socket. Run never returns until
// ctx is cancelled; every failure leads to a reconnect.
type Client struct {
	URL     string
	TLS     *tls.Config
	Hello   func() proto.Hello
	Handler Handler
	Log     *slog.Logger

	// Tunables with defaults set in Run.
	DialTimeout    time.Duration // 15s
	SilenceTimeout time.Duration // 45s
	SleepSlack     time.Duration // 30s
	Now            func() time.Time
	Backoff        *Backoff
}

type reason int

const (
	reasonDialFailed reason = iota
	reasonError
	reasonSilence
	reasonSleep
	reasonRejected
)

func (c *Client) defaults() {
	if c.DialTimeout == 0 {
		c.DialTimeout = 15 * time.Second
	}
	if c.SilenceTimeout == 0 {
		c.SilenceTimeout = 45 * time.Second
	}
	if c.SleepSlack == 0 {
		c.SleepSlack = 30 * time.Second
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Backoff == nil {
		c.Backoff = NewBackoff(time.Second, 60*time.Second, 0.2)
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
}

// Run is the agent's main loop.
func (c *Client) Run(ctx context.Context) {
	c.defaults()
	for {
		r := c.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		var wait time.Duration
		switch r {
		case reasonSleep:
			c.Backoff.Reset()
			c.Log.Info("resume from sleep detected; redialing now")
		case reasonRejected:
			c.Backoff.Saturate()
			wait = c.Backoff.Next()
		default:
			wait = c.Backoff.Next()
		}
		if wait > 0 {
			c.Log.Info("reconnecting", "in", wait.Round(time.Millisecond))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (c *Client) runOnce(ctx context.Context) reason {
	dctx, cancel := context.WithTimeout(ctx, c.DialTimeout)
	conn, _, err := websocket.Dial(dctx, c.URL, &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: c.TLS}},
	})
	cancel()
	if err != nil {
		c.Log.Warn("dial failed", "url", c.URL, "err", err)
		return reasonDialFailed
	}
	conn.SetReadLimit(proto.MaxMessageSize)
	defer conn.CloseNow()
	s := &session{conn: conn}

	hello, _ := proto.New(proto.TypeHello, c.Hello())
	if err := s.Send(hello); err != nil {
		return reasonError
	}
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	resp, err := s.read(hctx)
	cancel()
	if err != nil {
		c.Log.Warn("no handshake response", "err", err)
		return reasonError
	}
	var ack proto.HelloAck
	switch resp.Type {
	case proto.TypeHelloAck:
		if err := resp.Unmarshal(&ack); err != nil {
			return reasonError
		}
	case proto.TypeError:
		var e proto.Error
		resp.Unmarshal(&e)
		c.Log.Error("handshake rejected by server", "code", e.Code, "message", e.Message)
		return reasonRejected
	default:
		return reasonError
	}
	c.Backoff.Reset()
	hb := time.Duration(ack.HeartbeatIntervalS) * time.Second
	if hb <= 0 {
		hb = 15 * time.Second
	}
	c.Log.Info("connected", "session", ack.SessionID, "metrics_interval_s", ack.MetricsIntervalS)
	c.Handler.OnConnected(ack, s)
	defer c.Handler.OnDisconnected()

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan reason, 2)

	go func() { // sleep detector
		last := c.Now()
		t := time.NewTicker(hb)
		defer t.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-t.C:
				now := c.Now()
				if sleepDetected(last, now, hb, c.SleepSlack) {
					result <- reasonSleep
					conn.Close(websocket.StatusGoingAway, "resume from sleep")
					return
				}
				last = now
			}
		}
	}()

	go func() { // reader
		for {
			rctx, cancel := context.WithTimeout(sctx, c.SilenceTimeout)
			env, err := s.read(rctx)
			cancel()
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) && sctx.Err() == nil {
					c.Log.Warn("no traffic from server; reconnecting", "silence", c.SilenceTimeout)
					result <- reasonSilence
				} else {
					c.Log.Warn("connection closed", "err", err)
					result <- reasonError
				}
				return
			}
			if env.Type == proto.TypePing {
				pong, _ := proto.Reply(env, proto.TypePong, nil)
				s.Send(pong)
				continue
			}
			c.Handler.OnMessage(env, s)
		}
	}()

	select {
	case r := <-result:
		return r
	case <-ctx.Done():
		return reasonError
	}
}

type session struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

// Send writes one envelope. When it returns without error the frame has been
// handed to the kernel, which is what expect_disconnect commands rely on.
func (s *session) Send(env *proto.Envelope) error {
	b, err := proto.Encode(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.conn.Write(ctx, websocket.MessageText, b)
}

func (s *session) read(ctx context.Context) (*proto.Envelope, error) {
	_, data, err := s.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	return proto.Decode(data)
}
```

**Step 4: Run tests**

Run: `go test ./agent/transport/ && go vet ./...`
Expected: PASS

**Step 5: Commit**

```bash
git add agent/transport/
git commit -m "feat(agent): transport with jittered backoff, silence and sleep detection"
```

---

### Task 12: `agent/app`, `kuma-agent run`, and `kumaboard device add`

The design puts device registration in the dashboard (task 18). A CLI for the same operation is added here so the first real connection can be made before the UI exists, and so a restore drill can register a device over SSH. It calls the same store method.

**Files:**
- Create: `agent/app/app.go`
- Create: `cmd/kuma-agent/main.go`
- Create: `cmd/kumaboard/device.go`
- Modify: `cmd/kumaboard/main.go` (add `device` subcommand)
- Modify: `cmd/kumaboard/serve.go` (mount the hub)
- Create: `configs/kuma-agent.macos.example.yaml`

**Step 1: Agent app**

`agent/app/app.go`:

```go
// Package app is the agent's behaviour on top of the transport.
package app

import (
	"context"
	"log/slog"
	"runtime"
	"sync"

	"github.com/jhyoong/KumaBoard/agent/config"
	"github.com/jhyoong/KumaBoard/agent/transport"
	"github.com/jhyoong/KumaBoard/internal/buildinfo"
	"github.com/jhyoong/KumaBoard/proto"
)

// App implements transport.Handler.
type App struct {
	cfg *config.Config
	log *slog.Logger

	mu     sync.Mutex
	cancel context.CancelFunc // stops per-connection goroutines
}

// New creates the app.
func New(cfg *config.Config, log *slog.Logger) *App {
	return &App{cfg: cfg, log: log}
}

// Hello builds the handshake payload.
func (a *App) Hello() proto.Hello {
	return proto.Hello{
		ProtocolVersion: proto.Version,
		AgentVersion:    buildinfo.Version,
		DeviceName:      a.cfg.Device.Name,
		Token:           a.cfg.Token,
		OS:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		Capabilities:    a.cfg.Capabilities,
		Commands:        a.cfg.CommandDefs(),
	}
}

// OnConnected starts per-connection work. Task 14 adds the metrics ticker here.
func (a *App) OnConnected(ack proto.HelloAck, send transport.Sender) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
	_, a.cancel = context.WithCancel(context.Background())
}

// OnDisconnected stops per-connection work.
func (a *App) OnDisconnected() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
}

// OnMessage handles server messages. Task 20 adds command_request here.
func (a *App) OnMessage(env *proto.Envelope, send transport.Sender) {
	a.log.Debug("unhandled message", "type", env.Type)
}
```

**Step 2: Agent main**

`cmd/kuma-agent/main.go`:

```go
// Command kuma-agent connects a device to the KumaBoard control plane.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jhyoong/KumaBoard/agent/app"
	"github.com/jhyoong/KumaBoard/agent/config"
	"github.com/jhyoong/KumaBoard/agent/transport"
	"github.com/jhyoong/KumaBoard/internal/buildinfo"
	"github.com/jhyoong/KumaBoard/proto"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage: kuma-agent <command> [flags]

commands:
  run        connect to the control plane (flags: -config PATH)
  install    install the Windows service (Windows only)
  uninstall  remove the Windows service (Windows only)
  version    print version and protocol version`)
	os.Exit(2)
}

func defaultConfigPath() string {
	if p := os.Getenv("KUMA_AGENT_CONFIG"); p != "" {
		return p
	}
	return platformDefaultConfig
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runAgent(os.Args[2:])
	case "install":
		err = runInstall(os.Args[2:])
	case "uninstall":
		err = runUninstall(os.Args[2:])
	case "version":
		fmt.Printf("kuma-agent %s protocol %d\n", buildinfo.Version, proto.Version)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func runAgent(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	path := fs.String("config", defaultConfigPath(), "path to config.yaml")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runWithConfig(ctx, cfg, log)
}

// runWithConfig is shared by the foreground command and the Windows service.
func runWithConfig(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	a := app.New(cfg, log)
	client := &transport.Client{
		URL:     cfg.Server.URL,
		TLS:     &tls.Config{RootCAs: cfg.CAPool, MinVersion: tls.VersionTLS12},
		Hello:   a.Hello,
		Handler: a,
		Log:     log,
	}
	log.Info("kuma-agent starting", "device", cfg.Device.Name, "version", buildinfo.Version, "server", cfg.Server.URL)
	client.Run(ctx)
	return nil
}
```

`cmd/kuma-agent/platform_unix.go`:

```go
//go:build !windows

package main

const platformDefaultConfig = "/etc/kuma-agent/config.yaml"

func runInstall(args []string) error   { return errNotWindows }
func runUninstall(args []string) error { return errNotWindows }
```

`cmd/kuma-agent/platform_windows.go` (the service body is added in task 25; for now the stubs keep the build green):

```go
//go:build windows

package main

const platformDefaultConfig = `C:\ProgramData\kuma-agent\config.yaml`

func runInstall(args []string) error   { return errNotImplemented }
func runUninstall(args []string) error { return errNotImplemented }
```

`cmd/kuma-agent/errors.go`:

```go
package main

import "errors"

var (
	errNotWindows     = errors.New("this command is only available on Windows")
	errNotImplemented = errors.New("not implemented yet")
)
```

**Step 3: Device CLI and mount the hub**

`cmd/kumaboard/device.go`:

```go
package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/jhyoong/KumaBoard/server/store"
)

// runDeviceAdd registers a device from the command line and prints its token once.
func runDeviceAdd(args []string) error {
	fs := flag.NewFlagSet("device add", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to config.yaml")
	mac := fs.String("mac", "", "MAC address for Wake-on-LAN (optional)")
	normallyOff := fs.Bool("normally-off", false, "device is expected to be offline by default")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: kumaboard device add [-config PATH] [-mac MAC] [-normally-off] NAME")
	}
	cfg, err := loadConfigFlag([]string{"-config", *cfgPath})
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DataDir + "/kumaboard.db")
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	d, token, err := st.CreateDevice(ctx, fs.Arg(0), *mac, *normallyOff, store.Schedule{})
	if err != nil {
		return err
	}
	st.Audit(ctx, "cli", "device_add", d.Name, "ok", "")
	fmt.Printf("device %s registered\ntoken (shown once):\n%s\n", d.Name, token)
	return nil
}
```

In `cmd/kumaboard/main.go` add to the switch:

```go
	case "device":
		if len(os.Args) < 3 || os.Args[2] != "add" {
			usage()
		}
		err = runDeviceAdd(os.Args[3:])
```

and add `  device add     register a device and print its token once` to the usage text.

In `cmd/kumaboard/serve.go`, replace `buildHandler` with:

```go
func buildHandler(cfg *config.Config, st *store.Store, log *slog.Logger) (http.Handler, error) {
	h := hub.New(hub.Options{
		Store:             st,
		Events:            noEvents{},
		Log:               log,
		MetricsInterval:   time.Duration(cfg.MetricsIntervalS) * time.Second,
		HeartbeatInterval: time.Duration(cfg.HeartbeatIntervalS) * time.Second,
	})
	mux := http.NewServeMux()
	mux.Handle("GET /ws", h)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	return mux, nil
}

// noEvents is replaced by the registry in task 15.
type noEvents struct{}

func (noEvents) DeviceConnected(string)                 {}
func (noEvents) DeviceDisconnected(string)              {}
func (noEvents) MetricsReceived(string, proto.Metrics)  {}
func (noEvents) RunChanged(string)                      {}
```

Add the imports `github.com/jhyoong/KumaBoard/proto` and `github.com/jhyoong/KumaBoard/server/hub`.

`configs/kuma-agent.macos.example.yaml`:

```yaml
# /etc/kuma-agent/config.yaml  (macOS desktop)
device:
  name: macos-desktop

server:
  url: wss://192.168.1.150:8443/ws
  ca_file: /etc/kuma-agent/ca.pem

token_file: /etc/kuma-agent/token

capabilities:
  - metrics
  - custom-commands

commands:
  uptime:
    description: "Show uptime and load"
    run: ["/usr/bin/uptime"]
    timeout_s: 5
```

**Step 4: Build and connect an agent on the macOS desktop**

```bash
go build ./cmd/kumaboard && go build ./cmd/kuma-agent
./kumaboard serve -config /tmp/kb/config.yaml &
./kumaboard device add -config /tmp/kb/config.yaml macos-desktop
```

Copy the printed token into `/tmp/kb/agent/token`. Write `/tmp/kb/agent/config.yaml` like the example but with `url: wss://127.0.0.1:8443/ws`, `ca_file: /tmp/kb/data/pki/ca.pem`, `token_file: /tmp/kb/agent/token`. Then:

```bash
./kuma-agent run -config /tmp/kb/agent/config.yaml
```

Expected: agent logs `connected session=...`; server logs `agent connected device=macos-desktop`. Press Ctrl-C on the server: the agent logs `connection closed` and `reconnecting in 1s`, then `2s`, and so on. Restart the server: the agent reconnects. Stop both.

**Step 5: Commit**

```bash
git add agent/app/ cmd/ configs/
git commit -m "feat: kuma-agent run, device add CLI, hub mounted at /ws"
```

---

### Task 13: Integration harness and connection lifecycle tests

**Files:**
- Create: `internal/integration/harness_test.go`
- Create: `internal/integration/connection_test.go`

**Step 1: Write the harness**

`internal/integration/harness_test.go`:

```go
// Package integration starts a real server and real agents in one process.
package integration

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/agent/app"
	"github.com/jhyoong/KumaBoard/agent/config"
	"github.com/jhyoong/KumaBoard/agent/transport"
	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/hub"
	"github.com/jhyoong/KumaBoard/server/pki"
	"github.com/jhyoong/KumaBoard/server/store"
)

type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, s)
}
func (r *recorder) DeviceConnected(name string)                  { r.add("connect:" + name) }
func (r *recorder) DeviceDisconnected(name string)               { r.add("disconnect:" + name) }
func (r *recorder) MetricsReceived(name string, m proto.Metrics) { r.add("metrics:" + name) }
func (r *recorder) RunChanged(runID string)                      { r.add("run:" + runID) }
func (r *recorder) count(s string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e == s {
			n++
		}
	}
	return n
}

type harness struct {
	t      *testing.T
	dir    string
	st     *store.Store
	hub    *hub.Hub
	events *recorder
	log    *slog.Logger
	addr   string
	tlsCfg *tls.Config
	srv    *httptest.Server
	hubOpt hub.Options
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	if err := pki.Ensure(filepath.Join(dir, "pki"), []string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := pki.TLSConfig(filepath.Join(dir, "pki"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "kb.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	h := &harness{
		t: t, dir: dir, st: st, events: &recorder{}, addr: addr, tlsCfg: tlsCfg,
		log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	h.hubOpt = hub.Options{
		Store: st, Events: h.events, Log: h.log,
		MetricsInterval: 200 * time.Millisecond, HeartbeatInterval: 100 * time.Millisecond, PongTimeout: 100 * time.Millisecond,
	}
	h.startServer()
	return h
}

func (h *harness) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /ws", h.hub)
	return mux
}

func (h *harness) startServer() {
	h.t.Helper()
	h.hub = hub.New(h.hubOpt)
	ln, err := net.Listen("tcp", h.addr)
	if err != nil {
		h.t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h.handler())
	srv.Listener = ln
	srv.TLS = h.tlsCfg
	srv.StartTLS()
	h.srv = srv
	h.t.Cleanup(srv.Close)
}

func (h *harness) stopServer() {
	h.srv.CloseClientConnections()
	h.srv.Close()
}

func (h *harness) agentConfig(name, token string) *config.Config {
	cfg := &config.Config{}
	cfg.Device.Name = name
	cfg.Server.URL = "wss://" + h.addr + "/ws"
	cfg.Token = token
	cfg.Capabilities = []string{proto.CapMetrics, proto.CapCustomCommands}
	cfg.Commands = map[string]config.Command{}
	caPEM, _ := os.ReadFile(filepath.Join(h.dir, "pki", "ca.pem"))
	cfg.CAPool = poolFrom(caPEM)
	return cfg
}

// registerDevice creates a device and returns its token.
func (h *harness) registerDevice(name string) string {
	h.t.Helper()
	_, token, err := h.st.CreateDevice(context.Background(), name, "", false, store.Schedule{})
	if err != nil {
		h.t.Fatal(err)
	}
	return token
}

type agentHandle struct {
	app    *app.App
	client *transport.Client
	cancel context.CancelFunc
}

// startAgent runs a real agent in the background. tweak may adjust the client.
func (h *harness) startAgent(cfg *config.Config, tweak func(*transport.Client)) *agentHandle {
	h.t.Helper()
	a := app.New(cfg, h.log)
	c := &transport.Client{
		URL:            cfg.Server.URL,
		TLS:            &tls.Config{RootCAs: cfg.CAPool, MinVersion: tls.VersionTLS12},
		Hello:          a.Hello,
		Handler:        a,
		Log:            h.log,
		SilenceTimeout: 500 * time.Millisecond,
		Backoff:        transport.NewBackoff(50*time.Millisecond, 200*time.Millisecond, 0.2),
	}
	if tweak != nil {
		tweak(c)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	h.t.Cleanup(cancel)
	return &agentHandle{app: a, client: c, cancel: cancel}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
```

Add a tiny helper file `internal/integration/pool_test.go`:

```go
package integration

import "crypto/x509"

func poolFrom(pem []byte) *x509.CertPool {
	p := x509.NewCertPool()
	p.AppendCertsFromPEM(pem)
	return p
}
```

**Step 2: Write the connection tests**

`internal/integration/connection_test.go`:

```go
package integration

import (
	"context"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/agent/transport"
	"github.com/jhyoong/KumaBoard/proto"
)

func TestAgentConnects(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("a")
	h.startAgent(h.agentConfig("a", token), nil)
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("a") })
	d, _ := h.st.GetDevice(context.Background(), "a")
	if d.ProtocolVersion != proto.Version || d.LastSeen == nil {
		t.Fatalf("handshake not recorded: %+v", d)
	}
}

func TestRejectedAgentKeepsRetryingSlowly(t *testing.T) {
	h := newHarness(t)
	h.registerDevice("a")
	h.startAgent(h.agentConfig("a", "wrong-token"), nil)
	time.Sleep(600 * time.Millisecond)
	if h.hub.Connected("a") {
		t.Fatal("bad token connected")
	}
	d, _ := h.st.GetDevice(context.Background(), "a")
	if d.LastRejectReason != proto.ErrAuthFailed {
		t.Fatalf("reject reason %q", d.LastRejectReason)
	}
}

func TestUnsupportedProtocolIsVisible(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("a")
	cfg := h.agentConfig("a", token)
	h.startAgent(cfg, func(c *transport.Client) {
		inner := c.Hello
		c.Hello = func() proto.Hello {
			hl := inner()
			hl.ProtocolVersion = 99
			return hl
		}
	})
	waitFor(t, 3*time.Second, func() bool {
		d, _ := h.st.GetDevice(context.Background(), "a")
		return d.LastRejectReason == proto.ErrProtocolVersionUnsupported
	})
	if h.hub.Connected("a") {
		t.Fatal("incompatible agent was accepted")
	}
}

func TestDuplicateSessionKeepsOne(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("a")
	h.startAgent(h.agentConfig("a", token), nil)
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("a") })
	second := h.startAgent(h.agentConfig("a", token), nil)
	time.Sleep(500 * time.Millisecond)
	if n := h.hub.SessionCount(); n != 1 {
		t.Fatalf("sessions = %d, want 1", n)
	}
	second.cancel()
}

func TestReconnectAfterServerRestart(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("a")
	h.startAgent(h.agentConfig("a", token), nil)
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("a") })
	h.stopServer()
	time.Sleep(300 * time.Millisecond)
	h.startServer()
	waitFor(t, 5*time.Second, func() bool { return h.hub.Connected("a") })
	if h.events.count("connect:a") < 2 {
		t.Fatal("agent did not reconnect after restart")
	}
}

func TestSleepJumpRedials(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("a")
	var offset time.Duration
	var mu sync.Mutex
	h.startAgent(h.agentConfig("a", token), func(c *transport.Client) {
		c.Now = func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return time.Now().Add(offset)
		}
		c.SleepSlack = 200 * time.Millisecond
	})
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("a") })
	mu.Lock()
	offset = 5 * time.Second // simulate the wall clock jumping while asleep
	mu.Unlock()
	waitFor(t, 3*time.Second, func() bool { return h.events.count("connect:a") >= 2 })
}
```

Add `"sync"` to the imports of `connection_test.go`.

**Step 3: Run**

Run: `go test ./internal/integration/ -race -count=1`
Expected: PASS. If `TestReconnectAfterServerRestart` is flaky on port reuse, increase the sleep before `startServer` to 1s.

**Step 4: Commit**

```bash
git add internal/integration/
git commit -m "test: in-process integration harness and connection lifecycle tests"
```

---

## Build step 4: Metrics, state model, SSE, auth, first dashboard page

### Task 14: `agent/collectors` and the metrics ticker

**Files:**
- Create: `agent/collectors/collectors.go`
- Create: `agent/collectors/root_unix.go`
- Create: `agent/collectors/root_windows.go`
- Test: `agent/collectors/collectors_test.go`
- Modify: `agent/app/app.go`
- Create: `internal/integration/metrics_test.go`

**Step 1: Write the failing test**

`agent/collectors/collectors_test.go`:

```go
package collectors

import (
	"context"
	"testing"
)

func TestCollectReturnsRealValues(t *testing.T) {
	m, err := Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.MemTotalBytes == 0 || m.DiskTotalBytes == 0 || m.UptimeS == 0 {
		t.Fatalf("zero values in metrics: %+v", m)
	}
	if m.MemUsedPercent < 0 || m.MemUsedPercent > 100 || m.DiskUsedPercent < 0 || m.DiskUsedPercent > 100 {
		t.Fatalf("percent out of range: %+v", m)
	}
}
```

**Step 2: Run to verify failure**

Run: `go get github.com/shirou/gopsutil/v4@latest && go test ./agent/collectors/`
Expected: FAIL, undefined: Collect

**Step 3: Implement**

`agent/collectors/collectors.go`:

```go
// Package collectors samples CPU, memory, disk, uptime, and load.
package collectors

import (
	"context"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"

	"github.com/jhyoong/KumaBoard/proto"
)

// Collect takes one sample. Individual collectors that fail leave their fields
// zero rather than failing the whole sample; an error is returned only when
// nothing could be read.
func Collect(ctx context.Context) (proto.Metrics, error) {
	var m proto.Metrics
	okCount := 0
	// Interval 0 measures since the previous call, which is the metrics interval.
	if pct, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(pct) > 0 {
		m.CPUPercent = pct[0]
		okCount++
	}
	if v, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		m.MemTotalBytes, m.MemUsedBytes, m.MemUsedPercent = v.Total, v.Used, v.UsedPercent
		okCount++
	}
	if u, err := disk.UsageWithContext(ctx, rootPath); err == nil {
		m.DiskTotalBytes, m.DiskUsedBytes, m.DiskUsedPercent = u.Total, u.Used, u.UsedPercent
		okCount++
	}
	if up, err := host.UptimeWithContext(ctx); err == nil {
		m.UptimeS = up
		okCount++
	}
	if l, err := load.AvgWithContext(ctx); err == nil {
		m.Load1, m.Load5, m.Load15 = l.Load1, l.Load5, l.Load15
	}
	if okCount == 0 {
		return m, errNoCollectors
	}
	return m, nil
}
```

`agent/collectors/root_unix.go`:

```go
//go:build !windows

package collectors

import "errors"

const rootPath = "/"

var errNoCollectors = errors.New("collectors: every collector failed")
```

`agent/collectors/root_windows.go`:

```go
//go:build windows

package collectors

import "errors"

const rootPath = `C:\`

var errNoCollectors = errors.New("collectors: every collector failed")
```

**Step 4: Start the ticker from the app**

In `agent/app/app.go` replace `OnConnected` and add `metricsLoop`:

```go
// OnConnected starts the metrics ticker for this connection.
func (a *App) OnConnected(ack proto.HelloAck, send transport.Sender) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	interval := time.Duration(ack.MetricsIntervalS) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if a.has(proto.CapMetrics) {
		go a.metricsLoop(ctx, interval, send)
	}
}

func (a *App) has(cap string) bool {
	for _, c := range a.cfg.Capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

func (a *App) metricsLoop(ctx context.Context, interval time.Duration, send transport.Sender) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		m, err := collectors.Collect(cctx)
		cancel()
		if err != nil {
			a.log.Warn("metrics collection failed", "err", err)
		} else if env, err := proto.New(proto.TypeMetrics, m); err == nil {
			if err := send.Send(env); err != nil {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
```

Add imports `time` and `github.com/jhyoong/KumaBoard/agent/collectors`.

**Step 5: Integration test**

`internal/integration/metrics_test.go`:

```go
package integration

import (
	"testing"
	"time"
)

func TestMetricsArriveOnInterval(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("a")
	h.startAgent(h.agentConfig("a", token), nil)
	waitFor(t, 3*time.Second, func() bool { return h.events.count("metrics:a") >= 3 })
}
```

Run: `go test ./agent/collectors/ ./internal/integration/ -race -count=1`
Expected: PASS

**Step 6: Commit**

```bash
git add agent/ internal/integration/ go.mod go.sum
git commit -m "feat(agent): metrics collectors and per-connection ticker"
```

---

### Task 15: `server/registry` state model and ring buffer

**Files:**
- Create: `server/registry/ring.go`
- Create: `server/registry/state.go`
- Create: `server/registry/registry.go`
- Test: `server/registry/state_test.go`
- Test: `server/registry/registry_test.go`

**Step 1: Write the failing tests**

`server/registry/state_test.go`:

```go
package registry

import (
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

func at(t *testing.T, day string, hm string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04", day+" "+hm, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestInWindowDaily(t *testing.T) {
	sched := store.Schedule{ExpectedOffline: []store.Window{{Days: "*", From: "01:00", To: "05:00"}}, GracePeriodS: 300}
	day := "2026-09-21" // a Monday
	cases := map[string]bool{"00:54": false, "00:56": true, "03:00": true, "05:04": true, "05:06": false, "12:00": false}
	for hm, want := range cases {
		if got := InWindow(at(t, day, hm), sched); got != want {
			t.Errorf("%s: got %v want %v", hm, got, want)
		}
	}
}

func TestInWindowDaysAndWrap(t *testing.T) {
	mon := store.Schedule{ExpectedOffline: []store.Window{{Days: "mon", From: "01:00", To: "05:00"}}}
	if !InWindow(at(t, "2026-09-21", "03:00"), mon) {
		t.Error("Monday 03:00 should match")
	}
	if InWindow(at(t, "2026-09-22", "03:00"), mon) {
		t.Error("Tuesday 03:00 should not match a mon-only window")
	}
	wrap := store.Schedule{ExpectedOffline: []store.Window{{Days: "*", From: "23:00", To: "02:00"}}}
	if !InWindow(at(t, "2026-09-22", "01:00"), wrap) || !InWindow(at(t, "2026-09-21", "23:30"), wrap) {
		t.Error("window crossing midnight not matched")
	}
	if InWindow(at(t, "2026-09-22", "03:00"), wrap) {
		t.Error("03:00 outside 23:00-02:00")
	}
}

func TestDerive(t *testing.T) {
	now := time.Now()
	interval := 30 * time.Second
	sched := store.Schedule{}
	if s := Derive(now, true, now.Add(-10*time.Second), interval, false, sched); s != Online {
		t.Errorf("connected with fresh metrics: %s", s)
	}
	if s := Derive(now, true, now.Add(-2*time.Minute), interval, false, sched); s != Stale {
		t.Errorf("connected with old metrics: %s", s)
	}
	if s := Derive(now, false, now, interval, false, sched); s != OfflineUnexpected {
		t.Errorf("disconnected, no schedule: %s", s)
	}
	if s := Derive(now, false, now, interval, true, sched); s != OfflineExpected {
		t.Errorf("normally_off: %s", s)
	}
}

func TestRing(t *testing.T) {
	r := NewRing(3)
	if r.Latest() != nil || len(r.All()) != 0 {
		t.Fatal("empty ring not empty")
	}
	for i := 1; i <= 4; i++ {
		r.Push(proto.Metrics{UptimeS: uint64(i)})
	}
	all := r.All()
	if len(all) != 3 || all[0].UptimeS != 2 || all[2].UptimeS != 4 || r.Latest().UptimeS != 4 {
		t.Fatalf("ring order wrong: %+v", all)
	}
}
```

`server/registry/registry_test.go`:

```go
package registry

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

type capture struct {
	mu     sync.Mutex
	events []string
}

func (c *capture) Publish(name string, data any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, name)
}

func (c *capture) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.events {
		if e == name {
			n++
		}
	}
	return n
}

func TestRegistryFlow(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	st.CreateDevice(ctx, "a", "", false, store.Schedule{})
	st.CreateDevice(ctx, "b", "", true, store.Schedule{})
	pub := &capture{}
	r := New(st, pub, 30*time.Second)
	if err := r.Load(ctx); err != nil {
		t.Fatal(err)
	}
	sums, _ := r.Summaries(ctx)
	if len(sums) != 2 || sums[0].State != OfflineUnexpected || sums[1].State != OfflineExpected {
		t.Fatalf("initial states: %+v", sums)
	}
	r.DeviceConnected("a")
	s, _ := r.Summary(ctx, "a")
	if s.State != Online || !s.Connected {
		t.Fatalf("after connect: %+v", s)
	}
	r.MetricsReceived("a", proto.Metrics{CPUPercent: 5})
	s, _ = r.Summary(ctx, "a")
	if s.Metrics == nil || s.Metrics.CPUPercent != 5 || len(r.Metrics("a")) != 1 {
		t.Fatalf("metrics not stored: %+v", s)
	}
	r.DeviceDisconnected("a")
	s, _ = r.Summary(ctx, "a")
	if s.State != OfflineUnexpected {
		t.Fatalf("after disconnect: %+v", s)
	}
	if pub.count("device") < 3 || pub.count("metrics") != 1 {
		t.Fatalf("events: %v", pub.events)
	}
}

func TestSummaryFlagsIncompatible(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	ctx := context.Background()
	st.CreateDevice(ctx, "a", "", false, store.Schedule{})
	st.RecordReject(ctx, "a", proto.ErrProtocolVersionUnsupported)
	r := New(st, &capture{}, 30*time.Second)
	r.Load(ctx)
	s, _ := r.Summary(ctx, "a")
	if !s.Incompatible {
		t.Fatalf("expected incompatible flag: %+v", s)
	}
}
```

**Step 2: Run to verify failure**

Run: `go test ./server/registry/`
Expected: FAIL, undefined: InWindow

**Step 3: Implement**

`server/registry/ring.go`:

```go
package registry

import "github.com/jhyoong/KumaBoard/proto"

// Ring holds the last N metrics samples for a device.
type Ring struct {
	buf  []proto.Metrics
	n    int
	next int
}

// NewRing creates a ring of the given capacity.
func NewRing(size int) *Ring {
	return &Ring{buf: make([]proto.Metrics, size)}
}

// Push adds a sample, evicting the oldest when full.
func (r *Ring) Push(m proto.Metrics) {
	r.buf[r.next] = m
	r.next = (r.next + 1) % len(r.buf)
	if r.n < len(r.buf) {
		r.n++
	}
}

// Latest returns the newest sample or nil.
func (r *Ring) Latest() *proto.Metrics {
	if r.n == 0 {
		return nil
	}
	m := r.buf[(r.next-1+len(r.buf))%len(r.buf)]
	return &m
}

// All returns samples oldest first.
func (r *Ring) All() []proto.Metrics {
	out := make([]proto.Metrics, 0, r.n)
	start := (r.next - r.n + len(r.buf)) % len(r.buf)
	for i := 0; i < r.n; i++ {
		out = append(out, r.buf[(start+i)%len(r.buf)])
	}
	return out
}
```

`server/registry/state.go`:

```go
package registry

import (
	"fmt"
	"strings"
	"time"

	"github.com/jhyoong/KumaBoard/server/store"
)

// State is a device's derived status.
type State string

const (
	Online            State = "online"
	Stale             State = "stale"
	OfflineExpected   State = "offline_expected"
	OfflineUnexpected State = "offline_unexpected"
)

var dayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

func parseHM(s string) (int, bool) {
	var h, m int
	if n, err := fmt.Sscanf(s, "%d:%d", &h, &m); n != 2 || err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

func dayMatches(spec string, d time.Weekday) bool {
	if strings.TrimSpace(spec) == "*" || spec == "" {
		return true
	}
	for _, part := range strings.Split(spec, ",") {
		if wd, ok := dayNames[strings.ToLower(strings.TrimSpace(part))]; ok && wd == d {
			return true
		}
	}
	return false
}

// InWindow reports whether now falls inside any expected-offline window,
// widened by the grace period on both sides. Windows are in now's location,
// which is the server's local time. A window whose To is not after From
// crosses midnight and is attributed to the day it starts.
func InWindow(now time.Time, sched store.Schedule) bool {
	grace := time.Duration(sched.GracePeriodS) * time.Second
	for _, w := range sched.ExpectedOffline {
		from, ok1 := parseHM(w.From)
		to, ok2 := parseHM(w.To)
		if !ok1 || !ok2 {
			continue
		}
		dur := time.Duration(to-from) * time.Minute
		if dur <= 0 {
			dur += 24 * time.Hour
		}
		for _, dayOffset := range []int{0, -1} {
			day := time.Date(now.Year(), now.Month(), now.Day()+dayOffset, 0, 0, 0, 0, now.Location())
			if !dayMatches(w.Days, day.Weekday()) {
				continue
			}
			start := day.Add(time.Duration(from) * time.Minute)
			end := start.Add(dur)
			if !now.Before(start.Add(-grace)) && now.Before(end.Add(grace)) {
				return true
			}
		}
	}
	return false
}

// Derive computes the state from the raw facts.
func Derive(now time.Time, connected bool, lastMetrics time.Time, interval time.Duration, normallyOff bool, sched store.Schedule) State {
	if connected {
		if now.Sub(lastMetrics) > 3*interval {
			return Stale
		}
		return Online
	}
	if normallyOff || InWindow(now, sched) {
		return OfflineExpected
	}
	return OfflineUnexpected
}
```

`server/registry/registry.go`:

```go
// Package registry holds live device state derived from sessions and metrics.
package registry

import (
	"context"
	"sync"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

// Publisher receives dashboard events. The SSE broker implements it.
type Publisher interface {
	Publish(name string, data any)
}

// Summary is the device view sent to the dashboard.
type Summary struct {
	Name                string            `json:"name"`
	State               State             `json:"state"`
	Connected           bool              `json:"connected"`
	OS                  string            `json:"os"`
	Arch                string            `json:"arch"`
	AgentVersion        string            `json:"agent_version"`
	ProtocolVersion     int               `json:"protocol_version"`
	DesiredAgentVersion string            `json:"desired_agent_version"`
	Capabilities        []string          `json:"capabilities"`
	Commands            []proto.CommandDef `json:"commands"`
	MAC                 string            `json:"mac"`
	NormallyOff         bool              `json:"normally_off"`
	Schedule            store.Schedule    `json:"schedule"`
	LastSeen            *time.Time        `json:"last_seen"`
	LastDisconnectAt    *time.Time        `json:"last_disconnect_at"`
	Incompatible        bool              `json:"incompatible"`
	RejectReason        string            `json:"reject_reason"`
	Metrics             *proto.Metrics    `json:"metrics"`
}

// MetricsEvent is the payload of the "metrics" SSE event.
type MetricsEvent struct {
	Device  string        `json:"device"`
	Metrics proto.Metrics `json:"metrics"`
}

type entry struct {
	connected   bool
	lastMetrics time.Time
	ring        *Ring
	state       State
}

// Registry implements hub.Events and answers dashboard queries.
type Registry struct {
	st       *store.Store
	pub      Publisher
	interval time.Duration
	now      func() time.Time

	mu      sync.Mutex
	entries map[string]*entry
}

// New creates a registry. interval is the metrics interval.
func New(st *store.Store, pub Publisher, interval time.Duration) *Registry {
	return &Registry{st: st, pub: pub, interval: interval, now: time.Now, entries: map[string]*entry{}}
}

// Load creates entries for every stored device, all disconnected.
func (r *Registry) Load(ctx context.Context) error {
	devices, err := r.st.ListDevices(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range devices {
		r.ensureLocked(d.Name)
	}
	return nil
}

// Run re-evaluates states periodically so window edges and staleness are
// noticed without any message arriving. Blocks until ctx is done.
func (r *Registry) Run(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.reevaluateAll(ctx)
		}
	}
}

func (r *Registry) ensureLocked(name string) *entry {
	e, ok := r.entries[name]
	if !ok {
		e = &entry{ring: NewRing(60)}
		r.entries[name] = e
	}
	return e
}

// DeviceAdded registers a new device after creation via the API.
func (r *Registry) DeviceAdded(name string) {
	r.mu.Lock()
	r.ensureLocked(name)
	r.mu.Unlock()
	r.publishDevice(context.Background(), name)
}

// Refresh republishes a device after its settings changed.
func (r *Registry) Refresh(name string) {
	r.publishDevice(context.Background(), name)
}

// DeviceConnected implements hub.Events.
func (r *Registry) DeviceConnected(name string) {
	r.mu.Lock()
	e := r.ensureLocked(name)
	e.connected = true
	e.lastMetrics = r.now() // avoid a stale flicker before the first sample
	r.mu.Unlock()
	r.publishDevice(context.Background(), name)
}

// DeviceDisconnected implements hub.Events.
func (r *Registry) DeviceDisconnected(name string) {
	r.mu.Lock()
	e := r.ensureLocked(name)
	e.connected = false
	r.mu.Unlock()
	r.publishDevice(context.Background(), name)
}

// MetricsReceived implements hub.Events.
func (r *Registry) MetricsReceived(name string, m proto.Metrics) {
	r.mu.Lock()
	e := r.ensureLocked(name)
	e.lastMetrics = r.now()
	e.ring.Push(m)
	wasStale := e.state == Stale
	r.mu.Unlock()
	r.pub.Publish("metrics", MetricsEvent{Device: name, Metrics: m})
	if wasStale {
		r.publishDevice(context.Background(), name)
	}
}

// RunChanged implements hub.Events.
func (r *Registry) RunChanged(runID string) {
	run, err := r.st.GetRun(context.Background(), runID)
	if err != nil {
		return
	}
	r.pub.Publish("run", run)
}

// Metrics returns the ring buffer for a device, oldest first.
func (r *Registry) Metrics(name string) []proto.Metrics {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[name]
	if !ok {
		return []proto.Metrics{}
	}
	return e.ring.All()
}

// Summary builds the dashboard view of one device.
func (r *Registry) Summary(ctx context.Context, name string) (*Summary, error) {
	d, err := r.st.GetDevice(ctx, name)
	if err != nil {
		return nil, err
	}
	cmds, err := r.st.ListCommands(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	return r.summarise(d, cmds), nil
}

// Summaries builds the dashboard view of every device.
func (r *Registry) Summaries(ctx context.Context) ([]Summary, error) {
	devices, err := r.st.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(devices))
	for _, d := range devices {
		cmds, err := r.st.ListCommands(ctx, d.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, *r.summarise(d, cmds))
	}
	return out, nil
}

func (r *Registry) summarise(d *store.Device, cmds []proto.CommandDef) *Summary {
	r.mu.Lock()
	e := r.ensureLocked(d.Name)
	e.state = Derive(r.now(), e.connected, e.lastMetrics, r.interval, d.NormallyOff, d.Schedule)
	s := &Summary{
		Name: d.Name, State: e.state, Connected: e.connected,
		OS: d.OS, Arch: d.Arch, AgentVersion: d.AgentVersion, ProtocolVersion: d.ProtocolVersion,
		DesiredAgentVersion: d.DesiredAgentVersion, Capabilities: d.Capabilities, Commands: cmds,
		MAC: d.MAC, NormallyOff: d.NormallyOff, Schedule: d.Schedule,
		LastSeen: d.LastSeen, LastDisconnectAt: d.LastDisconnectAt,
		Incompatible: !e.connected && d.LastRejectReason == proto.ErrProtocolVersionUnsupported,
		RejectReason: d.LastRejectReason, Metrics: e.ring.Latest(),
	}
	r.mu.Unlock()
	if s.Schedule.ExpectedOffline == nil {
		s.Schedule.ExpectedOffline = []store.Window{}
	}
	return s
}

func (r *Registry) publishDevice(ctx context.Context, name string) {
	s, err := r.Summary(ctx, name)
	if err != nil {
		return
	}
	r.pub.Publish("device", s)
}

func (r *Registry) reevaluateAll(ctx context.Context) {
	devices, err := r.st.ListDevices(ctx)
	if err != nil {
		return
	}
	for _, d := range devices {
		r.mu.Lock()
		e := r.ensureLocked(d.Name)
		prev := e.state
		next := Derive(r.now(), e.connected, e.lastMetrics, r.interval, d.NormallyOff, d.Schedule)
		r.mu.Unlock()
		if next != prev {
			r.publishDevice(ctx, d.Name)
		}
	}
}
```

**Step 4: Run tests**

Run: `go test ./server/registry/ -race`
Expected: PASS

**Step 5: Commit**

```bash
git add server/registry/
git commit -m "feat(registry): four-state device model, downtime windows, metrics ring"
```

---

### Task 16: `server/sse` broker

**Files:**
- Create: `server/sse/sse.go`
- Test: `server/sse/sse_test.go`

**Step 1: Write the failing test**

`server/sse/sse_test.go`:

```go
package sse

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSubscribePublish(t *testing.T) {
	b := New()
	ch, cancel := b.Subscribe()
	defer cancel()
	b.Publish("device", map[string]string{"name": "a"})
	select {
	case ev := <-ch:
		if ev.Name != "device" {
			t.Fatalf("got %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
}

func TestServeHTTPStreams(t *testing.T) {
	b := New()
	srv := httptest.NewServer(b)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type %q", ct)
	}
	time.Sleep(50 * time.Millisecond) // let the subscription register
	b.Publish("device", map[string]string{"name": "a"})
	r := bufio.NewReader(resp.Body)
	var lines []string
	for len(lines) < 2 {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, ":") {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	if lines[0] != "event: device" || lines[1] != `data: {"name":"a"}` {
		t.Fatalf("got %q", lines)
	}
}
```

**Step 2: Run to verify failure**

Run: `go test ./server/sse/`
Expected: FAIL, undefined: New

**Step 3: Implement**

`server/sse/sse.go`:

```go
// Package sse fans dashboard events out to browsers over Server-Sent Events.
package sse

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Event is one named JSON payload.
type Event struct {
	Name string
	Data any
}

// Broker holds subscribers. Slow subscribers drop events rather than block
// publishers; the dashboard refetches on reconnect anyway.
type Broker struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// New creates a broker.
func New() *Broker {
	return &Broker{subs: map[chan Event]struct{}{}}
}

// Publish sends an event to every subscriber.
func (b *Broker) Publish(name string, data any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- Event{Name: name, Data: data}:
		default:
		}
	}
}

// Subscribe registers a subscriber. Call cancel to leave.
func (b *Broker) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// ServeHTTP streams events until the client disconnects.
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ch, cancel := b.Subscribe()
	defer cancel()
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev := <-ch:
			data, err := json.Marshal(ev.Data)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, data)
			flusher.Flush()
		}
	}
}
```

**Step 4: Run tests**

Run: `go test ./server/sse/`
Expected: PASS

**Step 5: Commit**

```bash
git add server/sse/
git commit -m "feat(sse): event broker and stream handler"
```

---

### Task 17: `server/auth` passwords, sessions, rate limiting, middleware, and `kumaboard passwd`

**Files:**
- Create: `server/auth/password.go`
- Create: `server/auth/limiter.go`
- Create: `server/auth/sessions.go`
- Create: `server/auth/middleware.go`
- Test: `server/auth/auth_test.go`
- Modify: `cmd/kumaboard/serve.go` (replace `runPasswd`)

**Step 1: Write the failing tests**

`server/auth/auth_test.go`:

```go
package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/server/store"
)

func TestPasswordHashVerify(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword("correct horse", h) || VerifyPassword("wrong", h) || VerifyPassword("x", "garbage") {
		t.Fatal("verify wrong")
	}
}

func TestLimiterLocksAfterFiveFailures(t *testing.T) {
	now := time.Now()
	l := NewLimiter(5, time.Minute)
	l.now = func() time.Time { return now }
	for i := 0; i < 4; i++ {
		l.Fail("1.2.3.4")
		if !l.Allowed("1.2.3.4") {
			t.Fatalf("locked after %d failures", i+1)
		}
	}
	l.Fail("1.2.3.4")
	if l.Allowed("1.2.3.4") {
		t.Fatal("not locked after 5 failures")
	}
	if !l.Allowed("5.6.7.8") {
		t.Fatal("other ip affected")
	}
	now = now.Add(61 * time.Second)
	if !l.Allowed("1.2.3.4") {
		t.Fatal("still locked after lockout expired")
	}
	l.Fail("1.2.3.4")
	l.Reset("1.2.3.4")
	for i := 0; i < 4; i++ {
		l.Fail("1.2.3.4")
	}
	if !l.Allowed("1.2.3.4") {
		t.Fatal("reset did not clear failures")
	}
}

func TestSessionsAndMiddleware(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	ctx := context.Background()
	st.UpsertUser(ctx, "admin", "x")
	uid, _, _ := st.GetUser(ctx, "admin")
	s := NewSessions(st)
	id, _, err := s.Create(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := RequireSession(s)(ok)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/devices", nil))
	if rec.Code != 401 || rec.Body.Len() != 0 {
		t.Fatalf("no cookie: code=%d body=%q", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest("GET", "/api/devices", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: id})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("valid cookie: code=%d", rec.Code)
	}
	s.Delete(ctx, id)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatal("deleted session still valid")
	}
}

func TestHostAndOriginChecks(t *testing.T) {
	allowed := map[string]bool{"192.168.1.5:8443": true, "kuma.local": true}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := HostCheck(allowed)(ok)
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "evil.example:8443"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("bad host accepted: %d", rec.Code)
	}
	req.Host = "192.168.1.5:8443"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("good host rejected: %d", rec.Code)
	}

	o := OriginCheck(allowed)(ok)
	req = httptest.NewRequest("GET", "/api/events", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	o.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bad origin accepted: %d", rec.Code)
	}
	req.Header.Set("Origin", "https://192.168.1.5:8443")
	rec = httptest.NewRecorder()
	o.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("good origin rejected: %d", rec.Code)
	}
	req.Header.Del("Origin")
	rec = httptest.NewRecorder()
	o.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("missing origin rejected: %d", rec.Code)
	}
}
```

**Step 2: Run to verify failure**

Run: `go get golang.org/x/crypto@latest golang.org/x/term@latest && go test ./server/auth/`
Expected: FAIL, undefined: HashPassword

**Step 3: Implement**

`server/auth/password.go`:

```go
// Package auth handles the operator login: argon2id passwords, cookie
// sessions, login rate limiting, and request guards.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32
)

// HashPassword returns a PHC-format argon2id string.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks a password against a HashPassword string.
func VerifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var mem, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, mem, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ErrBadCredentials is returned by Login on any failure.
var ErrBadCredentials = errors.New("auth: bad credentials")
```

`server/auth/limiter.go`:

```go
package auth

import (
	"sync"
	"time"
)

// Limiter locks out a source IP after repeated login failures.
type Limiter struct {
	max     int
	lockout time.Duration
	now     func() time.Time

	mu      sync.Mutex
	entries map[string]*bucket
}

type bucket struct {
	failures    int
	lockedUntil time.Time
}

// NewLimiter locks an IP for lockout after max failures.
func NewLimiter(max int, lockout time.Duration) *Limiter {
	return &Limiter{max: max, lockout: lockout, now: time.Now, entries: map[string]*bucket{}}
}

// Allowed reports whether the IP may attempt a login.
func (l *Limiter) Allowed(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.entries[ip]
	if !ok {
		return true
	}
	if !b.lockedUntil.IsZero() {
		if l.now().Before(b.lockedUntil) {
			return false
		}
		delete(l.entries, ip)
	}
	return true
}

// Fail records a failed attempt.
func (l *Limiter) Fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.entries[ip]
	if !ok {
		b = &bucket{}
		l.entries[ip] = b
	}
	b.failures++
	if b.failures >= l.max {
		b.failures = 0
		b.lockedUntil = l.now().Add(l.lockout)
	}
}

// Reset clears an IP after a successful login.
func (l *Limiter) Reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, ip)
}
```

`server/auth/sessions.go`:

```go
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"time"

	"github.com/jhyoong/KumaBoard/server/store"
)

// CookieName is the session cookie.
const CookieName = "kb_session"

// SessionTTL is how long a login lasts.
const SessionTTL = 12 * time.Hour

// Sessions creates and checks login sessions.
type Sessions struct {
	st *store.Store
}

// NewSessions wraps the store.
func NewSessions(st *store.Store) *Sessions { return &Sessions{st: st} }

// Create starts a session for a user.
func (s *Sessions) Create(ctx context.Context, userID int64) (string, time.Time, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	id := base64.RawURLEncoding.EncodeToString(b)
	exp := time.Now().Add(SessionTTL)
	if err := s.st.CreateSession(ctx, id, userID, exp); err != nil {
		return "", time.Time{}, err
	}
	return id, exp, nil
}

// Valid reports whether the session exists and has not expired.
func (s *Sessions) Valid(ctx context.Context, id string) bool {
	if id == "" {
		return false
	}
	_, exp, err := s.st.GetSession(ctx, id)
	return err == nil && time.Now().Before(exp)
}

// Delete ends a session.
func (s *Sessions) Delete(ctx context.Context, id string) error {
	return s.st.DeleteSession(ctx, id)
}

// SetCookie writes the session cookie.
func SetCookie(w http.ResponseWriter, id string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: id, Path: "/", Expires: expires,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

// ClearCookie removes the session cookie.
func ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}
```

`server/auth/middleware.go`:

```go
package auth

import (
	"net/http"
	"net/url"
)

// RequireSession returns 401 with an empty body when the cookie is missing or invalid.
func RequireSession(s *Sessions) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := r.Cookie(CookieName)
			if err != nil || !s.Valid(r.Context(), c.Value) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// HostCheck rejects requests whose Host header is not configured. This is
// the DNS-rebinding guard.
func HostCheck(allowed map[string]bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !allowed[r.Host] {
				http.Error(w, "unknown host", http.StatusMisdirectedRequest)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// OriginCheck rejects browser requests from another origin. Requests with no
// Origin header (agents, curl) pass; the Host check still applies to them.
func OriginCheck(allowed map[string]bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || !allowed[u.Host] {
					http.Error(w, "bad origin", http.StatusForbidden)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
```

**Step 4: `kumaboard passwd`**

Replace `runPasswd` in `cmd/kumaboard/serve.go`:

```go
func runPasswd(args []string) error {
	fs := flag.NewFlagSet("passwd", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to config.yaml")
	user := fs.String("user", "admin", "operator username")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	fmt.Fprint(os.Stderr, "New password: ")
	p1, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	fmt.Fprint(os.Stderr, "Repeat password: ")
	p2, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	if string(p1) != string(p2) {
		return errors.New("passwords do not match")
	}
	if len(p1) < 12 {
		return errors.New("password must be at least 12 characters")
	}
	hash, err := auth.HashPassword(string(p1))
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "kumaboard.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.UpsertUser(context.Background(), *user, hash); err != nil {
		return err
	}
	st.Audit(context.Background(), "cli", "passwd", *user, "ok", "")
	fmt.Printf("password set for %s\n", *user)
	return nil
}
```

Add imports `golang.org/x/term` and `github.com/jhyoong/KumaBoard/server/auth`.

**Step 5: Run tests**

Run: `go test ./server/auth/ && go build ./cmd/kumaboard && ./kumaboard passwd -config /tmp/kb/config.yaml`
Expected: tests PASS; passwd prompts twice and prints `password set for admin`.

**Step 6: Commit**

```bash
git add server/auth/ cmd/kumaboard/ go.mod go.sum
git commit -m "feat(auth): argon2id passwords, cookie sessions, rate limiter, host and origin guards, passwd CLI"
```

---

### Task 18: `server/api` REST routes and SSE endpoint

**Files:**
- Create: `server/api/api.go`
- Create: `server/api/json.go`
- Create: `server/api/devices.go`
- Create: `server/api/auth.go`
- Test: `server/api/api_test.go`
- Modify: `cmd/kumaboard/serve.go` (wire registry, broker, api, host check)

**Step 1: Write the failing test**

`server/api/api_test.go`:

```go
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/server/auth"
	"github.com/jhyoong/KumaBoard/server/hub"
	"github.com/jhyoong/KumaBoard/server/registry"
	"github.com/jhyoong/KumaBoard/server/sse"
	"github.com/jhyoong/KumaBoard/server/store"
)

type env struct {
	srv    *httptest.Server
	st     *store.Store
	client *http.Client
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hash, _ := auth.HashPassword("correct-horse-battery")
	st.UpsertUser(context.Background(), "admin", hash)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	broker := sse.New()
	reg := registry.New(st, broker, 30*time.Second)
	reg.Load(context.Background())
	h := hub.New(hub.Options{Store: st, Events: reg, Log: log, MetricsInterval: 30 * time.Second, HeartbeatInterval: 15 * time.Second})
	handler := New(Deps{
		Store: st, Registry: reg, Hub: h, Broker: broker,
		Sessions: auth.NewSessions(st), Limiter: auth.NewLimiter(5, time.Minute),
		AllowedHosts: map[string]bool{"127.0.0.1": true}, Log: log,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	jar := &cookieJar{}
	return &env{srv: srv, st: st, client: &http.Client{Jar: jar}}
}

func (e *env) do(t *testing.T, method, path string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (e *env) login(t *testing.T) {
	t.Helper()
	resp := e.do(t, "POST", "/api/login", map[string]string{"username": "admin", "password": "correct-horse-battery"})
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d", resp.StatusCode)
	}
}

func TestUnauthenticatedIs401(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/api/devices", "/api/audit", "/api/events", "/api/me"} {
		if resp := e.do(t, "GET", p, nil); resp.StatusCode != 401 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 5; i++ {
		resp := e.do(t, "POST", "/api/login", map[string]string{"username": "admin", "password": "wrong"})
		if resp.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i, resp.StatusCode)
		}
	}
	resp := e.do(t, "POST", "/api/login", map[string]string{"username": "admin", "password": "correct-horse-battery"})
	if resp.StatusCode != 429 {
		t.Fatalf("expected lockout, got %d", resp.StatusCode)
	}
	entries, _ := e.st.ListAudit(context.Background(), 10, 0)
	if len(entries) < 5 || entries[0].Action != "login" {
		t.Fatalf("login failures not audited: %+v", entries)
	}
}

func TestDeviceRegisterAndList(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	resp := e.do(t, "POST", "/api/devices", map[string]any{"name": "macos-desktop", "mac": "aa:bb:cc:dd:ee:ff", "normally_off": false})
	if resp.StatusCode != 201 {
		t.Fatalf("register: %d", resp.StatusCode)
	}
	var reg struct {
		Token  string           `json:"token"`
		Device registry.Summary `json:"device"`
	}
	json.NewDecoder(resp.Body).Decode(&reg)
	if reg.Token == "" || reg.Device.Name != "macos-desktop" || reg.Device.State != registry.OfflineUnexpected {
		t.Fatalf("bad register response: %+v", reg)
	}
	resp = e.do(t, "GET", "/api/devices", nil)
	var list []registry.Summary
	json.NewDecoder(resp.Body).Decode(&list)
	if len(list) != 1 || list[0].Name != "macos-desktop" {
		t.Fatalf("list: %+v", list)
	}
	resp = e.do(t, "PATCH", "/api/devices/macos-desktop", map[string]any{"mac": "11:22:33:44:55:66", "normally_off": true,
		"schedule": map[string]any{"expected_offline": []any{}, "grace_period_s": 60}})
	if resp.StatusCode != 200 {
		t.Fatalf("patch: %d", resp.StatusCode)
	}
	resp = e.do(t, "POST", "/api/devices/macos-desktop/revoke", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	d, _ := e.st.GetDevice(context.Background(), "macos-desktop")
	if d.TokenHash != nil || !d.NormallyOff {
		t.Fatalf("device not updated: %+v", d)
	}
}

func TestLogout(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	if resp := e.do(t, "GET", "/api/me", nil); resp.StatusCode != 200 {
		t.Fatal("me after login")
	}
	e.do(t, "POST", "/api/logout", nil)
	if resp := e.do(t, "GET", "/api/me", nil); resp.StatusCode != 401 {
		t.Fatal("me after logout")
	}
}
```

Add `server/api/jar_test.go` (a minimal cookie jar so the test client keeps the session cookie; `net/http/cookiejar` rejects `Secure` cookies over plain http, which the test server uses):

```go
package api

import (
	"net/http"
	"net/url"
	"sync"
)

type cookieJar struct {
	mu      sync.Mutex
	cookies []*http.Cookie
}

func (j *cookieJar) SetCookies(u *url.URL, cs []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, c := range cs {
		replaced := false
		for i, old := range j.cookies {
			if old.Name == c.Name {
				j.cookies[i] = c
				replaced = true
			}
		}
		if !replaced {
			j.cookies = append(j.cookies, c)
		}
	}
}

func (j *cookieJar) Cookies(u *url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []*http.Cookie
	for _, c := range j.cookies {
		if c.MaxAge >= 0 && c.Value != "" {
			out = append(out, c)
		}
	}
	return out
}
```

**Step 2: Run to verify failure**

Run: `go test ./server/api/`
Expected: FAIL, undefined: Deps, New

**Step 3: Implement**

`server/api/json.go`:

```go
package api

import (
	"encoding/json"
	"net/http"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
```

`server/api/api.go`:

```go
// Package api serves the dashboard's REST and SSE endpoints under /api.
package api

import (
	"log/slog"
	"net"
	"net/http"

	"github.com/jhyoong/KumaBoard/server/auth"
	"github.com/jhyoong/KumaBoard/server/hub"
	"github.com/jhyoong/KumaBoard/server/registry"
	"github.com/jhyoong/KumaBoard/server/sse"
	"github.com/jhyoong/KumaBoard/server/store"
)

// WakeFunc sends a magic packet. Set in task 23.
type WakeFunc func(mac string) error

// Deps are the collaborators the API needs.
type Deps struct {
	Store        *store.Store
	Registry     *registry.Registry
	Hub          *hub.Hub
	Broker       *sse.Broker
	Sessions     *auth.Sessions
	Limiter      *auth.Limiter
	AllowedHosts map[string]bool
	Log          *slog.Logger
	Wake         WakeFunc
}

type server struct {
	Deps
}

// New builds the /api handler. Every route except POST /api/login requires a session.
func New(d Deps) http.Handler {
	s := &server{Deps: d}
	authed := http.NewServeMux()
	authed.HandleFunc("POST /api/logout", s.logout)
	authed.HandleFunc("GET /api/me", s.me)
	authed.HandleFunc("GET /api/devices", s.listDevices)
	authed.HandleFunc("POST /api/devices", s.registerDevice)
	authed.HandleFunc("PATCH /api/devices/{name}", s.updateDevice)
	authed.HandleFunc("POST /api/devices/{name}/revoke", s.revokeDevice)
	authed.HandleFunc("GET /api/devices/{name}/metrics", s.deviceMetrics)
	authed.HandleFunc("GET /api/devices/{name}/commands", s.deviceCommands)
	authed.HandleFunc("GET /api/audit", s.listAudit)
	authed.Handle("GET /api/events", auth.OriginCheck(d.AllowedHosts)(d.Broker))
	s.mountRuns(authed) // task 21
	s.mountWake(authed) // task 23

	root := http.NewServeMux()
	root.HandleFunc("POST /api/login", s.login)
	root.Handle("/api/", auth.RequireSession(d.Sessions)(authed))
	return root
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Placeholders replaced in later tasks so this task compiles on its own.
func (s *server) mountRuns(mux *http.ServeMux) {}
func (s *server) mountWake(mux *http.ServeMux) {}
```

`server/api/auth.go`:

```go
package api

import (
	"net/http"

	"github.com/jhyoong/KumaBoard/server/auth"
	"github.com/jhyoong/KumaBoard/server/store"
)

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.Limiter.Allowed(ip) {
		s.Store.Audit(r.Context(), ip, "login", "", "locked_out", "")
		writeError(w, http.StatusTooManyRequests, "too many failed logins; try again later")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}
	id, hash, err := s.Store.GetUser(r.Context(), body.Username)
	if err != nil || !auth.VerifyPassword(body.Password, hash) {
		if err != nil && err != store.ErrNotFound {
			s.Log.Error("get user", "err", err)
		}
		s.Limiter.Fail(ip)
		s.Store.Audit(r.Context(), ip, "login", body.Username, "failed", "")
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	sid, exp, err := s.Sessions.Create(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session error")
		return
	}
	s.Limiter.Reset(ip)
	s.Store.Audit(r.Context(), body.Username, "login", body.Username, "ok", ip)
	auth.SetCookie(w, sid, exp)
	writeJSON(w, http.StatusOK, map[string]string{"username": body.Username})
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.CookieName); err == nil {
		s.Sessions.Delete(r.Context(), c.Value)
	}
	auth.ClearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"username": "admin"})
}

func (s *server) listAudit(w http.ResponseWriter, r *http.Request) {
	limit := intQuery(r, "limit", 100)
	before := int64(intQuery(r, "before", 0))
	entries, err := s.Store.ListAudit(r.Context(), limit, before)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if entries == nil {
		entries = []store.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}
```

`server/api/devices.go`:

```go
package api

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
var macRe = regexp.MustCompile(`^([0-9A-Fa-f]{2}[:-]){5}[0-9A-Fa-f]{2}$`)

type deviceBody struct {
	Name        string         `json:"name"`
	MAC         string         `json:"mac"`
	NormallyOff bool           `json:"normally_off"`
	Schedule    store.Schedule `json:"schedule"`
}

func (b *deviceBody) validate(requireName bool) error {
	if requireName && !nameRe.MatchString(b.Name) {
		return errors.New("name must be lowercase letters, digits, and hyphens")
	}
	if b.MAC != "" && !macRe.MatchString(b.MAC) {
		return errors.New("mac must look like aa:bb:cc:dd:ee:ff")
	}
	if b.Schedule.GracePeriodS < 0 {
		return errors.New("grace_period_s must not be negative")
	}
	for _, w := range b.Schedule.ExpectedOffline {
		if len(w.From) != 5 || len(w.To) != 5 {
			return errors.New("window times must be HH:MM")
		}
	}
	return nil
}

func (s *server) listDevices(w http.ResponseWriter, r *http.Request) {
	sums, err := s.Registry.Summaries(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sums)
}

func (s *server) registerDevice(w http.ResponseWriter, r *http.Request) {
	var body deviceBody
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}
	if err := body.validate(true); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	d, token, err := s.Store.CreateDevice(r.Context(), body.Name, body.MAC, body.NormallyOff, body.Schedule)
	if errors.Is(err, store.ErrExists) {
		writeError(w, http.StatusConflict, "device already exists")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Registry.DeviceAdded(d.Name)
	s.Store.Audit(r.Context(), "admin", "device_register", d.Name, "ok", "")
	sum, _ := s.Registry.Summary(r.Context(), d.Name)
	writeJSON(w, http.StatusCreated, map[string]any{"device": sum, "token": token})
}

func (s *server) updateDevice(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body deviceBody
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}
	if err := body.validate(false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Store.UpdateDeviceSettings(r.Context(), name, body.MAC, body.NormallyOff, body.Schedule); err != nil {
		s.notFoundOr500(w, err)
		return
	}
	s.Registry.Refresh(name)
	s.Store.Audit(r.Context(), "admin", "device_update", name, "ok", "")
	sum, _ := s.Registry.Summary(r.Context(), name)
	writeJSON(w, http.StatusOK, sum)
}

func (s *server) revokeDevice(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.Store.RevokeToken(r.Context(), name); err != nil {
		s.notFoundOr500(w, err)
		return
	}
	s.Hub.CloseDevice(name, "token revoked")
	s.Store.Audit(r.Context(), "admin", "token_revoke", name, "ok", "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (s *server) deviceMetrics(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.Store.GetDevice(r.Context(), name); err != nil {
		s.notFoundOr500(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Registry.Metrics(name))
}

func (s *server) deviceCommands(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.GetDevice(r.Context(), r.PathValue("name"))
	if err != nil {
		s.notFoundOr500(w, err)
		return
	}
	cmds, err := s.Store.ListCommands(r.Context(), d.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cmds == nil {
		cmds = []proto.CommandDef{}
	}
	writeJSON(w, http.StatusOK, cmds)
}

func (s *server) notFoundOr500(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func intQuery(r *http.Request, key string, def int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return def
	}
	return v
}
```

**Step 4: Wire it in `serve.go`**

Replace `buildHandler` and delete `noEvents`:

```go
func buildHandler(cfg *config.Config, st *store.Store, log *slog.Logger) (http.Handler, error) {
	broker := sse.New()
	reg := registry.New(st, broker, time.Duration(cfg.MetricsIntervalS)*time.Second)
	if err := reg.Load(context.Background()); err != nil {
		return nil, err
	}
	go reg.Run(context.Background())

	h := hub.New(hub.Options{
		Store:             st,
		Events:            reg,
		Log:               log,
		MetricsInterval:   time.Duration(cfg.MetricsIntervalS) * time.Second,
		HeartbeatInterval: time.Duration(cfg.HeartbeatIntervalS) * time.Second,
	})
	allowed := cfg.AllowedHosts()
	apiHandler := api.New(api.Deps{
		Store: st, Registry: reg, Hub: h, Broker: broker,
		Sessions: auth.NewSessions(st), Limiter: auth.NewLimiter(5, time.Minute),
		AllowedHosts: allowed, Log: log,
	})

	mux := http.NewServeMux()
	mux.Handle("GET /ws", auth.OriginCheck(allowed)(h))
	mux.Handle("/api/", apiHandler)
	mux.Handle("/", api.Static()) // task 19
	return auth.HostCheck(allowed)(mux), nil
}
```

Until task 19 exists, use `mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })` in place of the `api.Static()` line. Add imports for `api`, `auth`, `registry`, `sse`. Also start a goroutine that calls `st.DeleteExpiredSessions` hourly:

```go
	go func() {
		for {
			time.Sleep(time.Hour)
			st.DeleteExpiredSessions(context.Background())
		}
	}()
```

**Step 5: Run tests and smoke test**

Run: `go test ./server/... && go build ./cmd/kumaboard`

Then with the server running on `/tmp/kb/config.yaml`:

```bash
curl -s --cacert /tmp/kb/data/pki/ca.pem -c /tmp/kb/jar -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"<the password you set>"}' https://127.0.0.1:8443/api/login
curl -s --cacert /tmp/kb/data/pki/ca.pem -b /tmp/kb/jar https://127.0.0.1:8443/api/devices
curl -s --cacert /tmp/kb/data/pki/ca.pem -H 'Host: evil.example' https://127.0.0.1:8443/api/devices -o /dev/null -w '%{http_code}\n'
```

Expected: login returns `{"username":"admin"}`, devices returns a JSON array containing `macos-desktop`, the bad Host returns `421`. With the agent from task 12 running, the device shows `"state":"online"`.

**Step 6: Commit**

```bash
git add server/api/ cmd/kumaboard/
git commit -m "feat(api): login, devices, metrics, audit, SSE endpoint; wire registry and host check"
```

---

### Task 19: Frontend scaffold, login, devices page, embedding

**Files:**
- Create: `web/` (Vite project)
- Create: `web/embed.go`
- Create: `server/api/static.go`
- Modify: `cmd/kumaboard/serve.go`

**Step 1: Scaffold**

```bash
cd /path/to/KumaBoard
npm create vite@latest web -- --template react-ts
cd web
npm install
npm install react-router
npm install -D tailwindcss @tailwindcss/vite vitest
rm -f src/App.css src/assets/react.svg public/vite.svg
```

If `npm create vite` asks interactive questions, answer: framework React, variant TypeScript, no extra tooling.

`web/vite.config.ts`:

```ts
/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    // Dev only: proxy API calls to a locally running kumaboard.
    proxy: {
      '/api': { target: 'https://127.0.0.1:8443', secure: false },
    },
  },
  test: { environment: 'node' },
})
```

Add to `web/package.json` scripts: `"test": "vitest run"`.

`web/src/index.css`:

```css
@import "tailwindcss";
```

**Step 2: API client and state reducer with its test**

`web/src/api.ts`:

```ts
export type Metrics = {
  cpu_percent: number
  mem_total_bytes: number
  mem_used_bytes: number
  mem_used_percent: number
  disk_total_bytes: number
  disk_used_bytes: number
  disk_used_percent: number
  uptime_s: number
  load1: number
  load5: number
  load15: number
}

export type CommandDef = {
  name: string
  description: string
  timeout_s: number
  expect_disconnect: boolean
}

export type Window = { days: string; from: string; to: string }
export type Schedule = { expected_offline: Window[]; grace_period_s: number }

export type DeviceState = 'online' | 'stale' | 'offline_expected' | 'offline_unexpected'

export type Device = {
  name: string
  state: DeviceState
  connected: boolean
  os: string
  arch: string
  agent_version: string
  protocol_version: number
  desired_agent_version: string
  capabilities: string[]
  commands: CommandDef[]
  mac: string
  normally_off: boolean
  schedule: Schedule
  last_seen: string | null
  last_disconnect_at: string | null
  incompatible: boolean
  reject_reason: string
  metrics: Metrics | null
}

export type Run = {
  id: string
  device: string
  command: string
  requested_by: string
  requested_at: string
  started_at: string | null
  finished_at: string | null
  exit_code: number | null
  status: string
  stdout_tail: string
  stderr_tail: string
  truncated: boolean
}

export type AuditEntry = {
  id: number
  ts: string
  actor: string
  action: string
  target: string
  result: string
  detail: string
}

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message)
  }
}

export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', ...(init?.headers ?? {}) },
    ...init,
  })
  if (!res.ok) {
    let msg = res.statusText
    try {
      const body = await res.json()
      if (body?.error) msg = body.error
    } catch {
      /* no body */
    }
    throw new ApiError(res.status, msg)
  }
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}
```

`web/src/state.ts`:

```ts
import type { Device, Metrics, Run } from './api'

export type State = {
  devices: Record<string, Device>
  runs: Record<string, Run[]> // by device name, newest first
}

export type SSEEvent =
  | { type: 'device'; data: Device }
  | { type: 'metrics'; data: { device: string; metrics: Metrics } }
  | { type: 'run'; data: Run }

export const initialState: State = { devices: {}, runs: {} }

export function reduce(state: State, ev: SSEEvent): State {
  switch (ev.type) {
    case 'device':
      return { ...state, devices: { ...state.devices, [ev.data.name]: ev.data } }
    case 'metrics': {
      const d = state.devices[ev.data.device]
      if (!d) return state
      return { ...state, devices: { ...state.devices, [ev.data.device]: { ...d, metrics: ev.data.metrics } } }
    }
    case 'run': {
      const list = state.runs[ev.data.device] ?? []
      const idx = list.findIndex((r) => r.id === ev.data.id)
      const next = idx >= 0 ? list.map((r, i) => (i === idx ? ev.data : r)) : [ev.data, ...list].slice(0, 50)
      return { ...state, runs: { ...state.runs, [ev.data.device]: next } }
    }
  }
}

export function loadDevices(state: State, devices: Device[]): State {
  const map: Record<string, Device> = {}
  for (const d of devices) map[d.name] = d
  return { ...state, devices: map }
}
```

`web/src/state.test.ts`:

```ts
import { describe, expect, it } from 'vitest'
import { initialState, loadDevices, reduce } from './state'
import type { Device, Run } from './api'

const dev = (name: string, over: Partial<Device> = {}): Device => ({
  name, state: 'offline_unexpected', connected: false, os: '', arch: '', agent_version: '',
  protocol_version: 0, desired_agent_version: '', capabilities: [], commands: [], mac: '',
  normally_off: false, schedule: { expected_offline: [], grace_period_s: 0 },
  last_seen: null, last_disconnect_at: null, incompatible: false, reject_reason: '', metrics: null, ...over,
})

const run = (id: string, over: Partial<Run> = {}): Run => ({
  id, device: 'a', command: 'x', requested_by: 'admin', requested_at: '', started_at: null,
  finished_at: null, exit_code: null, status: 'running', stdout_tail: '', stderr_tail: '', truncated: false, ...over,
})

describe('reduce', () => {
  it('upserts devices', () => {
    let s = loadDevices(initialState, [dev('a')])
    s = reduce(s, { type: 'device', data: dev('a', { state: 'online' }) })
    s = reduce(s, { type: 'device', data: dev('b') })
    expect(s.devices.a.state).toBe('online')
    expect(Object.keys(s.devices)).toEqual(['a', 'b'])
  })

  it('attaches metrics to a known device and ignores unknown', () => {
    const m = { cpu_percent: 1, mem_total_bytes: 0, mem_used_bytes: 0, mem_used_percent: 0, disk_total_bytes: 0,
      disk_used_bytes: 0, disk_used_percent: 0, uptime_s: 0, load1: 0, load5: 0, load15: 0 }
    let s = loadDevices(initialState, [dev('a')])
    s = reduce(s, { type: 'metrics', data: { device: 'a', metrics: m } })
    expect(s.devices.a.metrics?.cpu_percent).toBe(1)
    expect(reduce(s, { type: 'metrics', data: { device: 'zz', metrics: m } })).toBe(s)
  })

  it('prepends new runs and replaces existing ones by id', () => {
    let s = reduce(initialState, { type: 'run', data: run('1') })
    s = reduce(s, { type: 'run', data: run('2') })
    s = reduce(s, { type: 'run', data: run('1', { status: 'ok' }) })
    expect(s.runs.a.map((r) => r.id)).toEqual(['2', '1'])
    expect(s.runs.a[1].status).toBe('ok')
  })
})
```

Run: `cd web && npm test`
Expected: 3 tests pass.

**Step 3: Views**

`web/src/useEvents.ts`:

```ts
import { useEffect, useReducer } from 'react'
import { api, type Device } from './api'
import { initialState, loadDevices, reduce, type SSEEvent, type State } from './state'

type Action = SSEEvent | { type: 'load'; devices: Device[] }

function reducer(state: State, action: Action): State {
  if (action.type === 'load') return loadDevices(state, action.devices)
  return reduce(state, action)
}

/** Fetches the device list once and then applies SSE events. */
export function useEvents(onUnauthorized: () => void) {
  const [state, dispatch] = useReducer(reducer, initialState)

  useEffect(() => {
    let es: EventSource | null = null
    let closed = false
    const start = async () => {
      try {
        const devices = await api<Device[]>('/api/devices')
        if (closed) return
        dispatch({ type: 'load', devices })
      } catch (e) {
        if ((e as { status?: number }).status === 401) onUnauthorized()
        return
      }
      es = new EventSource('/api/events')
      for (const t of ['device', 'metrics', 'run'] as const) {
        es.addEventListener(t, (ev) => dispatch({ type: t, data: JSON.parse((ev as MessageEvent).data) }))
      }
      es.onerror = () => {
        // EventSource retries on its own; refetch the list when it reconnects.
        es?.addEventListener('open', () => api<Device[]>('/api/devices').then((d) => dispatch({ type: 'load', devices: d })).catch(() => {}), { once: true })
      }
    }
    start()
    return () => {
      closed = true
      es?.close()
    }
  }, [onUnauthorized])

  return state
}
```

`web/src/format.ts`:

```ts
export function bytes(n: number): string {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let i = 0
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024
    i++
  }
  return `${n.toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}

export function uptime(s: number): string {
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  return d > 0 ? `${d}d ${h}h` : h > 0 ? `${h}h ${m}m` : `${m}m`
}

export function when(iso: string | null): string {
  if (!iso) return 'never'
  return new Date(iso).toLocaleString()
}
```

`web/src/components/StateBadge.tsx`:

```tsx
import type { DeviceState } from '../api'

const styles: Record<DeviceState, string> = {
  online: 'bg-green-100 text-green-800',
  stale: 'bg-yellow-100 text-yellow-800',
  offline_expected: 'bg-gray-200 text-gray-700',
  offline_unexpected: 'bg-red-100 text-red-800',
}

const labels: Record<DeviceState, string> = {
  online: 'online',
  stale: 'stale',
  offline_expected: 'offline (expected)',
  offline_unexpected: 'offline',
}

export function StateBadge({ state }: { state: DeviceState }) {
  return <span className={`rounded px-2 py-0.5 text-xs font-medium ${styles[state]}`}>{labels[state]}</span>
}
```

`web/src/components/DeviceCard.tsx`:

```tsx
import { Link } from 'react-router'
import type { Device } from '../api'
import { bytes, uptime, when } from '../format'
import { StateBadge } from './StateBadge'

export function DeviceCard({ device, onWake }: { device: Device; onWake?: (name: string) => void }) {
  const m = device.metrics
  return (
    <div className="rounded-lg border border-gray-200 bg-white p-4 shadow-sm">
      <div className="flex items-center justify-between">
        <Link to={`/devices/${device.name}`} className="text-lg font-semibold hover:underline">
          {device.name}
        </Link>
        <StateBadge state={device.state} />
      </div>
      <div className="mt-1 text-xs text-gray-500">
        {device.os}/{device.arch} · agent {device.agent_version || '?'} · proto {device.protocol_version || '?'}
      </div>
      {device.incompatible && (
        <div className="mt-2 rounded bg-red-50 p-2 text-sm text-red-700">Incompatible agent, needs redeploy</div>
      )}
      {m ? (
        <dl className="mt-3 grid grid-cols-2 gap-x-4 gap-y-1 text-sm">
          <dt className="text-gray-500">CPU</dt>
          <dd>{m.cpu_percent.toFixed(0)}%</dd>
          <dt className="text-gray-500">Memory</dt>
          <dd>{m.mem_used_percent.toFixed(0)}% of {bytes(m.mem_total_bytes)}</dd>
          <dt className="text-gray-500">Disk</dt>
          <dd>{m.disk_used_percent.toFixed(0)}% of {bytes(m.disk_total_bytes)}</dd>
          <dt className="text-gray-500">Uptime</dt>
          <dd>{uptime(m.uptime_s)}</dd>
        </dl>
      ) : (
        <div className="mt-3 text-sm text-gray-500">Last seen {when(device.last_seen)}</div>
      )}
      {device.capabilities.includes('wol-target') && onWake && (
        <button
          className="mt-3 rounded bg-blue-600 px-3 py-1 text-sm text-white hover:bg-blue-700 disabled:opacity-50"
          disabled={device.connected}
          onClick={() => onWake(device.name)}
        >
          Wake
        </button>
      )}
    </div>
  )
}
```

`web/src/views/Login.tsx`:

```tsx
import { useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router'
import { api, ApiError } from '../api'

export function Login() {
  const [username, setUsername] = useState('admin')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const navigate = useNavigate()

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setError('')
    try {
      await api('/api/login', { method: 'POST', body: JSON.stringify({ username, password }) })
      navigate('/')
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'login failed')
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-gray-50">
      <form onSubmit={submit} className="w-80 space-y-4 rounded-lg bg-white p-6 shadow">
        <h1 className="text-xl font-semibold">KumaBoard</h1>
        <input className="w-full rounded border p-2" value={username} onChange={(e) => setUsername(e.target.value)} placeholder="username" autoComplete="username" />
        <input className="w-full rounded border p-2" type="password" value={password} onChange={(e) => setPassword(e.target.value)} placeholder="password" autoComplete="current-password" />
        {error && <div className="text-sm text-red-600">{error}</div>}
        <button className="w-full rounded bg-blue-600 p-2 text-white hover:bg-blue-700">Log in</button>
      </form>
    </div>
  )
}
```

`web/src/views/Devices.tsx`:

```tsx
import { Link } from 'react-router'
import type { State } from '../state'
import { DeviceCard } from '../components/DeviceCard'

export function Devices({ state, onWake }: { state: State; onWake: (name: string) => void }) {
  const devices = Object.values(state.devices).sort((a, b) => a.name.localeCompare(b.name))
  return (
    <div>
      <div className="mb-4 flex items-center justify-between">
        <h1 className="text-2xl font-semibold">Devices</h1>
        <Link to="/register" className="rounded bg-blue-600 px-3 py-1 text-sm text-white hover:bg-blue-700">Register device</Link>
      </div>
      {devices.length === 0 ? (
        <p className="text-gray-500">No devices registered.</p>
      ) : (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2 lg:grid-cols-3">
          {devices.map((d) => <DeviceCard key={d.name} device={d} onWake={onWake} />)}
        </div>
      )}
    </div>
  )
}
```

`web/src/views/Register.tsx`:

```tsx
import { useState, type FormEvent } from 'react'
import { Link } from 'react-router'
import { api, ApiError, type Device } from '../api'

export function Register() {
  const [name, setName] = useState('')
  const [mac, setMac] = useState('')
  const [normallyOff, setNormallyOff] = useState(false)
  const [token, setToken] = useState('')
  const [error, setError] = useState('')

  const submit = async (e: FormEvent) => {
    e.preventDefault()
    setError('')
    try {
      const res = await api<{ device: Device; token: string }>('/api/devices', {
        method: 'POST',
        body: JSON.stringify({ name, mac, normally_off: normallyOff, schedule: { expected_offline: [], grace_period_s: 300 } }),
      })
      setToken(res.token)
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'failed')
    }
  }

  if (token) {
    return (
      <div className="max-w-lg space-y-4">
        <h1 className="text-2xl font-semibold">Device {name} registered</h1>
        <p className="text-sm text-gray-600">This token is shown once. Copy it into the device's token file now.</p>
        <pre className="overflow-x-auto rounded bg-gray-900 p-3 text-sm text-gray-100">{token}</pre>
        <button className="rounded bg-blue-600 px-3 py-1 text-sm text-white" onClick={() => navigator.clipboard.writeText(token)}>Copy</button>
        <Link to="/" className="ml-3 text-sm text-blue-600 hover:underline">Back to devices</Link>
      </div>
    )
  }

  return (
    <form onSubmit={submit} className="max-w-lg space-y-4">
      <h1 className="text-2xl font-semibold">Register device</h1>
      <label className="block text-sm">Name<input className="mt-1 w-full rounded border p-2" value={name} onChange={(e) => setName(e.target.value)} placeholder="macos-desktop" /></label>
      <label className="block text-sm">MAC (for Wake-on-LAN, optional)<input className="mt-1 w-full rounded border p-2" value={mac} onChange={(e) => setMac(e.target.value)} placeholder="aa:bb:cc:dd:ee:ff" /></label>
      <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={normallyOff} onChange={(e) => setNormallyOff(e.target.checked)} />Normally off (offline never alerts)</label>
      {error && <div className="text-sm text-red-600">{error}</div>}
      <button className="rounded bg-blue-600 px-3 py-1 text-white">Register</button>
    </form>
  )
}
```

`web/src/App.tsx`:

```tsx
import { useCallback, useEffect, useState } from 'react'
import { BrowserRouter, Link, Route, Routes, useLocation, useNavigate } from 'react-router'
import { api } from './api'
import { useEvents } from './useEvents'
import { Login } from './views/Login'
import { Devices } from './views/Devices'
import { Register } from './views/Register'

function Shell() {
  const navigate = useNavigate()
  const location = useLocation()
  const [flash, setFlash] = useState('')
  const onUnauthorized = useCallback(() => navigate('/login'), [navigate])
  const state = useEvents(onUnauthorized)

  useEffect(() => {
    api('/api/me').catch(() => navigate('/login'))
  }, [navigate, location.pathname])

  const wake = async (name: string) => {
    try {
      await api(`/api/devices/${name}/wake`, { method: 'POST' })
      setFlash(`Magic packet sent to ${name}`)
    } catch (e) {
      setFlash(`Wake failed: ${(e as Error).message}`)
    }
    setTimeout(() => setFlash(''), 4000)
  }

  const logout = async () => {
    await api('/api/logout', { method: 'POST' })
    navigate('/login')
  }

  return (
    <div className="min-h-screen bg-gray-50">
      <nav className="flex items-center gap-6 border-b bg-white px-6 py-3">
        <Link to="/" className="font-semibold">KumaBoard</Link>
        <Link to="/audit" className="text-sm text-gray-600 hover:underline">Audit</Link>
        <button onClick={logout} className="ml-auto text-sm text-gray-600 hover:underline">Log out</button>
      </nav>
      {flash && <div className="bg-blue-50 px-6 py-2 text-sm text-blue-800">{flash}</div>}
      <main className="p-6">
        <Routes>
          <Route path="/" element={<Devices state={state} onWake={wake} />} />
          <Route path="/register" element={<Register />} />
        </Routes>
      </main>
    </div>
  )
}

export default function App() {
  return (
    <BrowserRouter>
      <Routes>
        <Route path="/login" element={<Login />} />
        <Route path="/*" element={<Shell />} />
      </Routes>
    </BrowserRouter>
  )
}
```

`web/src/main.tsx`:

```tsx
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './index.css'
import App from './App'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
```

Set the `<title>` in `web/index.html` to `KumaBoard`.

**Step 4: Embed and serve**

`web/embed.go`:

```go
// Package web embeds the built dashboard.
package web

import "embed"

// Dist is the Vite build output. Run `make web` before `go build`.
//
//go:embed all:dist
var Dist embed.FS
```

`server/api/static.go`:

```go
package api

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/jhyoong/KumaBoard/web"
)

// Static serves the embedded dashboard with an SPA fallback to index.html.
// The shell is public; every /api route still requires a session.
func Static() http.Handler {
	dist, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		panic(err)
	}
	files := http.FS(dist)
	fileServer := http.FileServer(files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean(r.URL.Path)
		if p != "/" {
			if f, err := files.Open(p); err == nil {
				f.Close()
				if strings.HasPrefix(p, "/assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}
```

In `cmd/kumaboard/serve.go` use `mux.Handle("/", api.Static())`.

**Step 5: Build and use it**

```bash
cd /path/to/KumaBoard
make web
go build ./cmd/kumaboard
./kumaboard serve -config /tmp/kb/config.yaml
```

Open `https://127.0.0.1:8443/` in a browser, accept the certificate warning, log in with the passwd password, and confirm the macOS desktop card shows `online` with metrics while the agent from task 12 runs. Stop the agent and confirm the card turns to `offline` within a few seconds without reloading. Register a second device through the form and confirm the token is displayed once.

For frontend iteration use `cd web && npm run dev` with the server running; the proxy forwards `/api`. Browsers treat `http://localhost` as a secure context, so the `Secure` cookie works there.

**Step 6: Commit**

```bash
git add web/ server/api/static.go cmd/kumaboard/serve.go
git commit -m "feat(web): login, devices grid, register flow, SSE state, embedded in server"
```

---

## Build step 5: Commands end to end

### Task 20: `agent/commands` runner and `command_request` handling

**Files:**
- Create: `agent/commands/runner.go`
- Create: `agent/commands/capwriter.go`
- Create: `agent/commands/env_unix.go`
- Create: `agent/commands/env_windows.go`
- Test: `agent/commands/runner_test.go`
- Modify: `agent/app/app.go`

**Step 1: Write the failing tests**

`agent/commands/runner_test.go`:

```go
package commands

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/agent/config"
	"github.com/jhyoong/KumaBoard/proto"
)

func newRunner(t *testing.T, cmds map[string]config.Command) *Runner {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin utilities")
	}
	r := New(cmds, t.TempDir(), slog.New(slog.NewTextHandler(os.Stderr, nil)))
	r.DispatchDelay = 10 * time.Millisecond
	return r
}

func collect(r *Runner, name string) []proto.CommandResult {
	var out []proto.CommandResult
	r.Execute(context.Background(), name, func(res proto.CommandResult) error {
		out = append(out, res)
		return nil
	})
	return out
}

func TestOKAndFailed(t *testing.T) {
	r := newRunner(t, map[string]config.Command{
		"echo": {Run: []string{"/bin/echo", "hi"}, TimeoutS: 5},
		"fail": {Run: []string{"/bin/sh", "-c", "echo oops >&2; exit 3"}, TimeoutS: 5},
	})
	res := collect(r, "echo")
	if len(res) != 1 || res[0].Status != proto.RunOK || res[0].Stdout != "hi\n" || res[0].ExitCode != 0 {
		t.Fatalf("echo: %+v", res)
	}
	res = collect(r, "fail")
	if res[0].Status != proto.RunFailed || res[0].ExitCode != 3 || !strings.Contains(res[0].Stderr, "oops") {
		t.Fatalf("fail: %+v", res)
	}
}

func TestUnknownAndMissingBinary(t *testing.T) {
	r := newRunner(t, map[string]config.Command{
		"gone": {Run: []string{"/nonexistent/bin"}, TimeoutS: 5},
	})
	if res := collect(r, "nope"); res[0].Status != proto.RunUnknownCommand {
		t.Fatalf("unknown: %+v", res)
	}
	if res := collect(r, "gone"); res[0].Status != proto.RunFailed || res[0].ExitCode != -1 {
		t.Fatalf("missing binary: %+v", res)
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	r := newRunner(t, map[string]config.Command{
		"slow": {Run: []string{"/bin/sh", "-c", "sleep 30 & wait"}, TimeoutS: 1},
	})
	start := time.Now()
	res := collect(r, "slow")
	if res[0].Status != proto.RunTimeout {
		t.Fatalf("timeout: %+v", res)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("timeout did not kill the process group promptly")
	}
}

func TestBusy(t *testing.T) {
	r := newRunner(t, map[string]config.Command{
		"slow": {Run: []string{"/bin/sleep", "1"}, TimeoutS: 5},
	})
	done := make(chan struct{})
	go func() {
		collect(r, "slow")
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	if res := collect(r, "slow"); res[0].Status != proto.RunBusy {
		t.Fatalf("busy: %+v", res)
	}
	<-done
	if res := collect(r, "slow"); res[0].Status != proto.RunOK {
		t.Fatalf("after release: %+v", res)
	}
}

func TestTruncation(t *testing.T) {
	r := newRunner(t, map[string]config.Command{
		"big": {Run: []string{"/bin/sh", "-c", "head -c 100000 /dev/zero | tr '\\0' a"}, TimeoutS: 5},
	})
	res := collect(r, "big")
	if !res[0].Truncated || len(res[0].Stdout) != MaxOutput {
		t.Fatalf("truncation: truncated=%v len=%d", res[0].Truncated, len(res[0].Stdout))
	}
}

func TestExpectDisconnect(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	r := newRunner(t, map[string]config.Command{
		"bye": {Run: []string{"/usr/bin/touch", marker}, TimeoutS: 5, ExpectDisconnect: true},
	})
	res := collect(r, "bye")
	if len(res) != 1 || res[0].Status != proto.RunDispatched {
		t.Fatalf("expect exactly one dispatched result: %+v", res)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("command did not run after dispatch")
	}
}

func TestFixedEnvironment(t *testing.T) {
	r := newRunner(t, map[string]config.Command{
		"env": {Run: []string{"/usr/bin/env"}, TimeoutS: 5},
	})
	t.Setenv("KUMA_LEAK", "1")
	res := collect(r, "env")
	if strings.Contains(res[0].Stdout, "KUMA_LEAK") || !strings.Contains(res[0].Stdout, "PATH=") {
		t.Fatalf("environment not fixed: %s", res[0].Stdout)
	}
}
```

**Step 2: Run to verify failure**

Run: `go test ./agent/commands/`
Expected: FAIL, undefined: New

**Step 3: Implement**

`agent/commands/capwriter.go`:

```go
package commands

import "bytes"

// MaxOutput caps stdout and stderr each.
const MaxOutput = 64 << 10

type capWriter struct {
	buf       bytes.Buffer
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	room := MaxOutput - w.buf.Len()
	if room <= 0 {
		w.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		w.buf.Write(p[:room])
		w.truncated = true
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

func (w *capWriter) String() string { return w.buf.String() }
```

`agent/commands/env_unix.go`:

```go
//go:build !windows

package commands

import (
	"os"
	"os/exec"
	"syscall"
)

func fixedEnv() []string {
	return []string{
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + os.Getenv("HOME"),
		"LANG=C.UTF-8",
	}
}

// setProcAttr puts the child in its own process group so a timeout kills
// everything it spawned, not only the direct child.
func setProcAttr(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
}
```

`agent/commands/env_windows.go`:

```go
//go:build windows

package commands

import (
	"os"
	"os/exec"
)

func fixedEnv() []string {
	root := os.Getenv("SYSTEMROOT")
	if root == "" {
		root = `C:\Windows`
	}
	return []string{
		"PATH=" + root + `\System32;` + root + `;` + root + `\System32\WindowsPowerShell\v1.0`,
		"SYSTEMROOT=" + root,
		"TEMP=" + os.Getenv("TEMP"),
		"TMP=" + os.Getenv("TMP"),
		"USERPROFILE=" + os.Getenv("USERPROFILE"),
	}
}

// setProcAttr is a no-op on Windows; exec.CommandContext kills the child.
func setProcAttr(c *exec.Cmd) {}
```

`agent/commands/runner.go`:

```go
// Package commands executes named commands from the local config.
package commands

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/jhyoong/KumaBoard/agent/config"
	"github.com/jhyoong/KumaBoard/proto"
)

// Runner executes commands by name. Names not in its map never execute.
type Runner struct {
	cmds    map[string]config.Command
	workDir string
	log     *slog.Logger

	// DispatchDelay is how long to wait after sending "dispatched" before an
	// expect_disconnect command runs, so the result reaches the server.
	DispatchDelay time.Duration

	mu      sync.Mutex
	running map[string]bool
}

// New creates a runner. workDir is the agent's binary directory.
func New(cmds map[string]config.Command, workDir string, log *slog.Logger) *Runner {
	return &Runner{cmds: cmds, workDir: workDir, log: log, DispatchDelay: 2 * time.Second, running: map[string]bool{}}
}

// Execute runs a command and emits exactly one result. For expect_disconnect
// commands that result is "dispatched", sent before the command starts.
func (r *Runner) Execute(ctx context.Context, name string, emit func(proto.CommandResult) error) {
	cmd, ok := r.cmds[name]
	if !ok {
		emit(proto.CommandResult{Status: proto.RunUnknownCommand})
		return
	}
	if !r.acquire(name) {
		emit(proto.CommandResult{Status: proto.RunBusy})
		return
	}
	defer r.release(name)

	if cmd.ExpectDisconnect {
		if err := emit(proto.CommandResult{Status: proto.RunDispatched}); err != nil {
			r.log.Warn("could not report dispatched; not running", "command", name, "err", err)
			return
		}
		time.Sleep(r.DispatchDelay)
		res := r.run(ctx, cmd)
		r.log.Info("expect_disconnect command finished", "command", name, "status", res.Status, "exit", res.ExitCode)
		return
	}
	res := r.run(ctx, cmd)
	if err := emit(res); err != nil {
		r.log.Warn("result discarded; session gone", "command", name, "status", res.Status)
	}
}

func (r *Runner) acquire(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running[name] {
		return false
	}
	r.running[name] = true
	return true
}

func (r *Runner) release(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.running, name)
}

func (r *Runner) run(ctx context.Context, cmd config.Command) proto.CommandResult {
	tctx, cancel := context.WithTimeout(ctx, time.Duration(cmd.TimeoutS)*time.Second)
	defer cancel()
	c := exec.CommandContext(tctx, cmd.Run[0], cmd.Run[1:]...)
	c.Dir = r.workDir
	c.Env = fixedEnv()
	stdout, stderr := &capWriter{}, &capWriter{}
	c.Stdout, c.Stderr = stdout, stderr
	c.WaitDelay = 2 * time.Second
	setProcAttr(c)

	start := time.Now()
	err := c.Run()
	res := proto.CommandResult{
		Stdout: stdout.String(), Stderr: stderr.String(),
		Truncated:  stdout.truncated || stderr.truncated,
		DurationMS: time.Since(start).Milliseconds(),
	}
	var exitErr *exec.ExitError
	switch {
	case errors.Is(tctx.Err(), context.DeadlineExceeded):
		res.Status, res.ExitCode = proto.RunTimeout, -1
	case err == nil:
		res.Status = proto.RunOK
	case errors.As(err, &exitErr):
		res.Status, res.ExitCode = proto.RunFailed, exitErr.ExitCode()
	default:
		res.Status, res.ExitCode = proto.RunFailed, -1
		res.Stderr += err.Error()
	}
	return res
}
```

**Step 4: Handle `command_request` in the app**

In `agent/app/app.go`, add a `runner *commands.Runner` field and a `DispatchDelay` override for tests, create the runner in `New`, and replace `OnMessage`:

```go
// New creates the app.
func New(cfg *config.Config, log *slog.Logger) *App {
	workDir := "."
	if exe, err := os.Executable(); err == nil {
		workDir = filepath.Dir(exe)
	}
	return &App{cfg: cfg, log: log, runner: commands.New(cfg.Commands, workDir, log)}
}

// SetDispatchDelay shortens the expect_disconnect flush wait. Tests only.
func (a *App) SetDispatchDelay(d time.Duration) { a.runner.DispatchDelay = d }

// OnMessage handles server messages.
func (a *App) OnMessage(env *proto.Envelope, send transport.Sender) {
	switch env.Type {
	case proto.TypeCommandRequest:
		var req proto.CommandRequest
		if err := env.Unmarshal(&req); err != nil {
			return
		}
		// Background context on purpose: a dropped session must not kill a
		// running command. The result is simply discarded if Send fails.
		go a.runner.Execute(context.Background(), req.Name, func(res proto.CommandResult) error {
			reply, err := proto.Reply(env, proto.TypeCommandResult, res)
			if err != nil {
				return err
			}
			return send.Send(reply)
		})
	default:
		a.log.Debug("unhandled message", "type", env.Type)
	}
}
```

Add imports `os`, `path/filepath`, and `github.com/jhyoong/KumaBoard/agent/commands`.

**Step 5: Run tests**

Run: `go test ./agent/... -race`
Expected: PASS

**Step 6: Commit**

```bash
git add agent/
git commit -m "feat(agent): argv command runner with busy, timeout, truncation, expect_disconnect"
```

---

### Task 21: Server command router, run API, and integration tests

**Files:**
- Create: `server/hub/router.go`
- Modify: `server/hub/hub.go`
- Create: `server/api/runs.go`
- Modify: `server/api/api.go` (remove the `mountRuns` placeholder)
- Create: `internal/integration/commands_test.go`

**Step 1: Write the failing integration tests**

`internal/integration/commands_test.go`:

```go
package integration

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/agent/config"
	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/hub"
)

func commandAgent(t *testing.T, h *harness) (*agentHandle, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin utilities")
	}
	token := h.registerDevice("a")
	cfg := h.agentConfig("a", token)
	cfg.Commands = map[string]config.Command{
		"echo": {Run: []string{"/bin/echo", "hi"}, TimeoutS: 5},
		"fail": {Run: []string{"/bin/sh", "-c", "exit 3"}, TimeoutS: 5},
		"slow": {Run: []string{"/bin/sleep", "3"}, TimeoutS: 1},
		"nap":  {Run: []string{"/bin/sleep", "1"}, TimeoutS: 5},
		"bye":  {Run: []string{"/bin/sleep", "0.2"}, TimeoutS: 5, ExpectDisconnect: true},
		"stay": {Run: []string{"/bin/true"}, TimeoutS: 1, ExpectDisconnect: true},
	}
	ag := h.startAgent(cfg, nil)
	ag.app.SetDispatchDelay(50 * time.Millisecond)
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("a") })
	return ag, "a"
}

func (h *harness) runStatus(id string) string {
	r, err := h.st.GetRun(context.Background(), id)
	if err != nil {
		return ""
	}
	return r.Status
}

func (h *harness) waitStatus(t *testing.T, id, want string, d time.Duration) {
	t.Helper()
	waitFor(t, d, func() bool { return h.runStatus(id) == want })
}

func TestCommandOK(t *testing.T) {
	h := newHarness(t)
	_, dev := commandAgent(t, h)
	id, err := h.hub.RequestCommand(context.Background(), dev, "echo", "test")
	if err != nil {
		t.Fatal(err)
	}
	h.waitStatus(t, id, proto.RunOK, 3*time.Second)
	r, _ := h.st.GetRun(context.Background(), id)
	if r.StdoutTail != "hi\n" || r.ExitCode == nil || *r.ExitCode != 0 || r.FinishedAt == nil {
		t.Fatalf("run: %+v", r)
	}
	if h.events.count("run:"+id) < 2 {
		t.Fatal("run change events not published")
	}
}

func TestCommandFailedAndTimeout(t *testing.T) {
	h := newHarness(t)
	_, dev := commandAgent(t, h)
	id, _ := h.hub.RequestCommand(context.Background(), dev, "fail", "test")
	h.waitStatus(t, id, proto.RunFailed, 3*time.Second)
	r, _ := h.st.GetRun(context.Background(), id)
	if r.ExitCode == nil || *r.ExitCode != 3 {
		t.Fatalf("exit code: %+v", r)
	}
	id, _ = h.hub.RequestCommand(context.Background(), dev, "slow", "test")
	h.waitStatus(t, id, proto.RunTimeout, 5*time.Second)
}

func TestCommandUnknownAndBusy(t *testing.T) {
	h := newHarness(t)
	_, dev := commandAgent(t, h)
	if _, err := h.hub.RequestCommand(context.Background(), dev, "nope", "test"); err != hub.ErrUnknownCommand {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := h.hub.RequestCommand(context.Background(), "ghost", "echo", "test"); err != hub.ErrNotConnected {
		t.Fatalf("not connected: %v", err)
	}
	first, _ := h.hub.RequestCommand(context.Background(), dev, "nap", "test")
	time.Sleep(100 * time.Millisecond)
	second, _ := h.hub.RequestCommand(context.Background(), dev, "nap", "test")
	h.waitStatus(t, second, proto.RunBusy, 3*time.Second)
	h.waitStatus(t, first, proto.RunOK, 3*time.Second)
}

func TestExpectDisconnectOutcomes(t *testing.T) {
	h := newHarness(t)
	ag, dev := commandAgent(t, h)
	id, _ := h.hub.RequestCommand(context.Background(), dev, "bye", "test")
	h.waitStatus(t, id, proto.RunDispatched, 3*time.Second)
	ag.cancel() // the machine "went to sleep"
	h.waitStatus(t, id, proto.RunDisconnectedAsExpected, 3*time.Second)

	h2 := newHarness(t)
	_, dev2 := commandAgent(t, h2)
	id2, _ := h2.hub.RequestCommand(context.Background(), dev2, "stay", "test")
	h2.waitStatus(t, id2, proto.RunDispatched, 3*time.Second)
	h2.waitStatus(t, id2, proto.RunNoDisconnect, 10*time.Second) // timeout_s 1 + 5s grace
}

func TestRunLostWhenAgentDies(t *testing.T) {
	h := newHarness(t)
	ag, dev := commandAgent(t, h)
	id, _ := h.hub.RequestCommand(context.Background(), dev, "nap", "test")
	time.Sleep(100 * time.Millisecond)
	ag.cancel()
	h.waitStatus(t, id, proto.RunLost, 3*time.Second)
}
```

**Step 2: Run to verify failure**

Run: `go test ./internal/integration/ -run Command -count=1`
Expected: FAIL, h.hub.RequestCommand undefined

**Step 3: Implement the router**

`server/hub/router.go`:

```go
package hub

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

var (
	ErrNotConnected   = errors.New("hub: device not connected")
	ErrUnknownCommand = errors.New("hub: command not declared by device")
)

const outputCap = 64 << 10

type inflight struct {
	runID      string
	sessionID  string
	deviceID   int64
	deviceName string
	command    string
	dispatched bool
	timer      *time.Timer
	stdout     strings.Builder
	stderr     strings.Builder
}

// router tracks in-flight runs. All fields are guarded by mu.
type router struct {
	mu   sync.Mutex
	runs map[string]*inflight // by run ID (== command_request envelope ID)
}

func newRouter() *router { return &router{runs: map[string]*inflight{}} }

func (r *router) add(f *inflight) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs[f.runID] = f
}

func (r *router) get(id string) *inflight {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs[id]
}

// remove takes a run out of flight. Returns false if it was already removed,
// which is how the timer and the result handler avoid finishing a run twice.
func (r *router) remove(f *inflight) (dispatched bool, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, present := r.runs[f.runID]; !present {
		return false, false
	}
	delete(r.runs, f.runID)
	return f.dispatched, true
}

func (r *router) markDispatched(f *inflight) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f.dispatched = true
}

func (r *router) appendOutput(f *inflight, stream, data string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := &f.stdout
	if stream == "stderr" {
		b = &f.stderr
	}
	if room := outputCap - b.Len(); room > 0 {
		if len(data) > room {
			data = data[:room]
		}
		b.WriteString(data)
	}
}

func (r *router) removeSession(sessionID string) []*inflight {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*inflight
	for id, f := range r.runs {
		if f.sessionID == sessionID {
			out = append(out, f)
			delete(r.runs, id)
		}
	}
	return out
}

// RequestCommand dispatches a named command to a connected device and returns the run ID.
func (h *Hub) RequestCommand(ctx context.Context, deviceName, command, requestedBy string) (string, error) {
	s := h.session(deviceName)
	if s == nil {
		return "", ErrNotConnected
	}
	cmds, err := h.opts.Store.ListCommands(ctx, s.DeviceID)
	if err != nil {
		return "", err
	}
	var def *proto.CommandDef
	for i := range cmds {
		if cmds[i].Name == command {
			def = &cmds[i]
		}
	}
	if def == nil {
		return "", ErrUnknownCommand
	}
	env, err := proto.New(proto.TypeCommandRequest, proto.CommandRequest{Name: command})
	if err != nil {
		return "", err
	}
	run := &store.Run{ID: env.ID, DeviceID: s.DeviceID, Command: command, RequestedBy: requestedBy,
		RequestedAt: time.Now(), Status: proto.RunRunning}
	if err := h.opts.Store.InsertRun(ctx, run); err != nil {
		return "", err
	}
	f := &inflight{runID: run.ID, sessionID: s.ID, deviceID: s.DeviceID, deviceName: deviceName, command: command}
	f.timer = time.AfterFunc(time.Duration(def.TimeoutS+5)*time.Second, func() { h.runTimedOut(f) })
	h.router.add(f)
	if err := s.Send(env); err != nil {
		h.router.remove(f)
		f.timer.Stop()
		h.opts.Store.FinishRun(ctx, run.ID, proto.RunLost, nil, "", "", false)
		h.opts.Events.RunChanged(run.ID)
		return run.ID, err
	}
	h.opts.Store.Audit(ctx, requestedBy, "command_request", deviceName+"/"+command, "sent", run.ID)
	h.opts.Events.RunChanged(run.ID)
	return run.ID, nil
}

func (h *Hub) runTimedOut(f *inflight) {
	dispatched, ok := h.router.remove(f)
	if !ok {
		return
	}
	f.timer.Stop()
	status := proto.RunTimeout
	if dispatched {
		status = proto.RunNoDisconnect
	}
	ctx := context.Background()
	h.opts.Store.FinishRun(ctx, f.runID, status, nil, f.stdout.String(), f.stderr.String(), false)
	h.opts.Store.Audit(ctx, "server", "command_result", f.deviceName+"/"+f.command, status, f.runID)
	h.opts.Events.RunChanged(f.runID)
}

func (h *Hub) handleCommandResult(ctx context.Context, s *Session, env *proto.Envelope) {
	f := h.router.get(env.ID)
	if f == nil || f.sessionID != s.ID {
		h.opts.Log.Warn("result for unknown run", "device", s.DeviceName, "id", env.ID)
		return
	}
	var res proto.CommandResult
	if err := env.Unmarshal(&res); err != nil {
		return
	}
	if res.Status == proto.RunDispatched {
		h.router.markDispatched(f)
		h.opts.Store.SetRunStatus(ctx, f.runID, proto.RunDispatched)
		h.opts.Events.RunChanged(f.runID)
		return
	}
	if _, ok := h.router.remove(f); !ok {
		return
	}
	f.timer.Stop()
	var exit *int
	if res.Status == proto.RunOK || res.Status == proto.RunFailed {
		v := res.ExitCode
		exit = &v
	}
	stdout, stderr := res.Stdout, res.Stderr
	if stdout == "" && stderr == "" {
		stdout, stderr = f.stdout.String(), f.stderr.String()
	}
	h.opts.Store.FinishRun(ctx, f.runID, res.Status, exit, stdout, stderr, res.Truncated)
	h.opts.Store.Audit(ctx, "agent:"+s.DeviceName, "command_result", s.DeviceName+"/"+f.command, res.Status, f.runID)
	h.opts.Events.RunChanged(f.runID)
}

func (h *Hub) handleCommandOutput(s *Session, env *proto.Envelope) {
	f := h.router.get(env.ID)
	if f == nil || f.sessionID != s.ID {
		return
	}
	var o proto.CommandOutput
	if err := env.Unmarshal(&o); err != nil {
		return
	}
	h.router.appendOutput(f, o.Stream, o.Data)
}

// sessionEnded finalises every run the session had in flight.
func (h *Hub) sessionEnded(ctx context.Context, s *Session) {
	for _, f := range h.router.removeSession(s.ID) {
		f.timer.Stop()
		status := proto.RunLost
		if f.dispatched {
			status = proto.RunDisconnectedAsExpected
		}
		h.opts.Store.FinishRun(ctx, f.runID, status, nil, f.stdout.String(), f.stderr.String(), false)
		h.opts.Store.Audit(ctx, "server", "command_result", f.deviceName+"/"+f.command, status, f.runID)
		h.opts.Events.RunChanged(f.runID)
	}
	h.opts.Store.MarkDeviceRunsLost(ctx, s.DeviceID)
}
```

In `server/hub/hub.go`:

- Add `router *router` to `Hub` and set `router: newRouter()` in `New`.
- Delete the stub `func (h *Hub) sessionEnded(ctx context.Context, s *Session) {}`.
- Add to the `switch` in `handleMessage`:

```go
	case proto.TypeCommandResult:
		h.handleCommandResult(ctx, s, env)
	case proto.TypeCommandOutput:
		h.handleCommandOutput(s, env)
```

**Step 4: Run API**

`server/api/runs.go`:

```go
package api

import (
	"errors"
	"net/http"

	"github.com/jhyoong/KumaBoard/server/hub"
	"github.com/jhyoong/KumaBoard/server/store"
)

func (s *server) mountRuns(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/devices/{name}/commands/{cmd}", s.runCommand)
	mux.HandleFunc("GET /api/devices/{name}/runs", s.listRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.getRun)
}

func (s *server) runCommand(w http.ResponseWriter, r *http.Request) {
	name, cmd := r.PathValue("name"), r.PathValue("cmd")
	id, err := s.Hub.RequestCommand(r.Context(), name, cmd, "admin")
	switch {
	case errors.Is(err, hub.ErrNotConnected):
		writeError(w, http.StatusConflict, "device is not connected")
	case errors.Is(err, hub.ErrUnknownCommand):
		writeError(w, http.StatusNotFound, "command not declared by device")
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"run_id": id})
	}
}

func (s *server) listRuns(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.GetDevice(r.Context(), r.PathValue("name"))
	if err != nil {
		s.notFoundOr500(w, err)
		return
	}
	runs, err := s.Store.ListRuns(r.Context(), d.ID, intQuery(r, "limit", 50))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *server) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.Store.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
}
```

Remove the `func (s *server) mountRuns(mux *http.ServeMux) {}` placeholder from `server/api/api.go`.

**Step 5: Run tests**

Run: `go test ./server/... ./internal/integration/ -race -count=1`
Expected: PASS. `TestExpectDisconnectOutcomes` takes about 7 seconds because of the 5 second grace.

**Step 6: Commit**

```bash
git add server/hub/ server/api/ internal/integration/
git commit -m "feat(hub): command router with timeouts, dispatched, lost; run API"
```

---

### Task 22: Device detail view with command buttons and run history

**Files:**
- Create: `web/src/views/DeviceDetail.tsx`
- Create: `web/src/components/RunList.tsx`
- Create: `web/src/components/Sparkline.tsx`
- Create: `web/src/components/ScheduleEditor.tsx`
- Modify: `web/src/App.tsx`

**Step 1: Components**

`web/src/components/Sparkline.tsx` (one tiny SVG polyline; if you extend this into real charts, read the dataviz skill first):

```tsx
export function Sparkline({ values, max = 100, label }: { values: number[]; max?: number; label: string }) {
  const w = 160
  const h = 32
  if (values.length < 2) return <div className="text-xs text-gray-400">{label}: collecting</div>
  const step = w / (values.length - 1)
  const pts = values.map((v, i) => `${(i * step).toFixed(1)},${(h - (Math.min(v, max) / max) * h).toFixed(1)}`).join(' ')
  return (
    <div className="flex items-center gap-2 text-xs text-gray-600">
      <span className="w-14">{label}</span>
      <svg width={w} height={h} className="text-blue-600" aria-label={label}>
        <polyline points={pts} fill="none" stroke="currentColor" strokeWidth="1.5" />
      </svg>
      <span>{values[values.length - 1].toFixed(0)}%</span>
    </div>
  )
}
```

`web/src/components/RunList.tsx`:

```tsx
import { useState } from 'react'
import type { Run } from '../api'
import { when } from '../format'

const colour: Record<string, string> = {
  ok: 'text-green-700', disconnected_as_expected: 'text-green-700',
  running: 'text-blue-700', dispatched: 'text-blue-700',
  failed: 'text-red-700', timeout: 'text-red-700', no_disconnect: 'text-red-700',
  unknown_command: 'text-red-700', busy: 'text-yellow-700', lost: 'text-gray-500',
}

function RunRow({ run }: { run: Run }) {
  const [open, setOpen] = useState(false)
  const hasOutput = run.stdout_tail || run.stderr_tail
  return (
    <li className="border-b py-2 text-sm">
      <div className="flex items-center gap-3">
        <span className="font-mono">{run.command}</span>
        <span className={`font-medium ${colour[run.status] ?? ''}`}>{run.status}</span>
        {run.exit_code !== null && <span className="text-gray-500">exit {run.exit_code}</span>}
        <span className="ml-auto text-gray-500">{when(run.requested_at)}</span>
        {hasOutput && <button className="text-blue-600 hover:underline" onClick={() => setOpen(!open)}>{open ? 'hide' : 'output'}</button>}
      </div>
      {open && (
        <div className="mt-2 space-y-1">
          {run.stdout_tail && <pre className="overflow-x-auto rounded bg-gray-900 p-2 text-xs text-gray-100">{run.stdout_tail}</pre>}
          {run.stderr_tail && <pre className="overflow-x-auto rounded bg-red-950 p-2 text-xs text-red-100">{run.stderr_tail}</pre>}
          {run.truncated && <div className="text-xs text-gray-500">output truncated at 64 KiB</div>}
        </div>
      )}
    </li>
  )
}

export function RunList({ runs }: { runs: Run[] }) {
  if (runs.length === 0) return <p className="text-sm text-gray-500">No runs yet.</p>
  return <ul>{runs.map((r) => <RunRow key={r.id} run={r} />)}</ul>
}
```

`web/src/components/ScheduleEditor.tsx`:

```tsx
import { useState } from 'react'
import type { Device, Schedule, Window } from '../api'

type Props = { device: Device; onSave: (body: { mac: string; normally_off: boolean; schedule: Schedule }) => Promise<void> }

export function ScheduleEditor({ device, onSave }: Props) {
  const [mac, setMac] = useState(device.mac)
  const [normallyOff, setNormallyOff] = useState(device.normally_off)
  const [grace, setGrace] = useState(device.schedule.grace_period_s)
  const [windows, setWindows] = useState<Window[]>(device.schedule.expected_offline)
  const [msg, setMsg] = useState('')

  const update = (i: number, patch: Partial<Window>) => setWindows(windows.map((w, j) => (j === i ? { ...w, ...patch } : w)))

  const save = async () => {
    try {
      await onSave({ mac, normally_off: normallyOff, schedule: { expected_offline: windows, grace_period_s: grace } })
      setMsg('saved')
    } catch (e) {
      setMsg((e as Error).message)
    }
  }

  return (
    <div className="space-y-3 text-sm">
      <label className="block">MAC<input className="mt-1 w-full rounded border p-1" value={mac} onChange={(e) => setMac(e.target.value)} /></label>
      <label className="flex items-center gap-2"><input type="checkbox" checked={normallyOff} onChange={(e) => setNormallyOff(e.target.checked)} />Normally off</label>
      <label className="block">Grace period (seconds)<input type="number" className="mt-1 w-32 rounded border p-1" value={grace} onChange={(e) => setGrace(Number(e.target.value))} /></label>
      <div>
        <div className="mb-1 font-medium">Expected offline windows</div>
        {windows.map((w, i) => (
          <div key={i} className="mb-1 flex gap-2">
            <input className="w-24 rounded border p-1" value={w.days} onChange={(e) => update(i, { days: e.target.value })} placeholder="* or mon,tue" />
            <input className="w-20 rounded border p-1" value={w.from} onChange={(e) => update(i, { from: e.target.value })} placeholder="01:00" />
            <input className="w-20 rounded border p-1" value={w.to} onChange={(e) => update(i, { to: e.target.value })} placeholder="05:00" />
            <button className="text-red-600" onClick={() => setWindows(windows.filter((_, j) => j !== i))}>remove</button>
          </div>
        ))}
        <button className="text-blue-600 hover:underline" onClick={() => setWindows([...windows, { days: '*', from: '01:00', to: '05:00' }])}>add window</button>
      </div>
      <div className="flex items-center gap-3">
        <button className="rounded bg-blue-600 px-3 py-1 text-white" onClick={save}>Save</button>
        <span className="text-gray-500">{msg}</span>
      </div>
    </div>
  )
}
```

**Step 2: The view**

`web/src/views/DeviceDetail.tsx`:

```tsx
import { useEffect, useState } from 'react'
import { useNavigate, useParams } from 'react-router'
import { api, type Metrics, type Run, type Schedule } from '../api'
import type { State } from '../state'
import { StateBadge } from '../components/StateBadge'
import { RunList } from '../components/RunList'
import { Sparkline } from '../components/Sparkline'
import { ScheduleEditor } from '../components/ScheduleEditor'
import { when } from '../format'

function mergeRuns(live: Run[], fetched: Run[]): Run[] {
  const byId = new Map<string, Run>()
  for (const r of fetched) byId.set(r.id, r)
  for (const r of live) byId.set(r.id, r)
  return [...byId.values()].sort((a, b) => b.requested_at.localeCompare(a.requested_at))
}

export function DeviceDetail({ state, onWake }: { state: State; onWake: (name: string) => void }) {
  const { name = '' } = useParams()
  const navigate = useNavigate()
  const device = state.devices[name]
  const [fetched, setFetched] = useState<Run[]>([])
  const [history, setHistory] = useState<Metrics[]>([])
  const [msg, setMsg] = useState('')

  useEffect(() => {
    api<Run[]>(`/api/devices/${name}/runs`).then(setFetched).catch(() => {})
    api<Metrics[]>(`/api/devices/${name}/metrics`).then(setHistory).catch(() => {})
  }, [name])

  useEffect(() => {
    if (device?.metrics) setHistory((h) => [...h, device.metrics!].slice(-60))
  }, [device?.metrics])

  if (!device) return <p className="text-gray-500">Unknown device.</p>
  const runs = mergeRuns(state.runs[name] ?? [], fetched)

  const run = async (cmd: string) => {
    try {
      await api(`/api/devices/${name}/commands/${cmd}`, { method: 'POST' })
      setMsg(`${cmd} requested`)
    } catch (e) {
      setMsg(`${cmd}: ${(e as Error).message}`)
    }
  }

  const save = async (body: { mac: string; normally_off: boolean; schedule: Schedule }) => {
    await api(`/api/devices/${name}`, { method: 'PATCH', body: JSON.stringify(body) })
  }

  const revoke = async () => {
    if (!confirm(`Revoke the token for ${name}? The agent will be disconnected and must be re-registered.`)) return
    await api(`/api/devices/${name}/revoke`, { method: 'POST' })
    navigate('/')
  }

  return (
    <div className="space-y-6">
      <div className="flex items-center gap-3">
        <h1 className="text-2xl font-semibold">{device.name}</h1>
        <StateBadge state={device.state} />
        <span className="text-sm text-gray-500">{device.os}/{device.arch} · agent {device.agent_version || '?'} · proto {device.protocol_version || '?'} · last seen {when(device.last_seen)}</span>
      </div>
      {device.incompatible && <div className="rounded bg-red-50 p-2 text-sm text-red-700">Incompatible agent, needs redeploy</div>}

      <section>
        <h2 className="mb-2 font-medium">Metrics</h2>
        <Sparkline label="CPU" values={history.map((m) => m.cpu_percent)} />
        <Sparkline label="Memory" values={history.map((m) => m.mem_used_percent)} />
        <Sparkline label="Disk" values={history.map((m) => m.disk_used_percent)} />
      </section>

      <section>
        <h2 className="mb-2 font-medium">Commands</h2>
        <div className="flex flex-wrap gap-2">
          {device.capabilities.includes('wol-target') && (
            <button className="rounded bg-blue-600 px-3 py-1 text-sm text-white disabled:opacity-50" disabled={device.connected} onClick={() => onWake(name)}>Wake</button>
          )}
          {device.commands.map((c) => (
            <button key={c.name} title={c.description} className="rounded border px-3 py-1 text-sm hover:bg-gray-100 disabled:opacity-50" disabled={!device.connected} onClick={() => run(c.name)}>
              {c.name}
            </button>
          ))}
          {device.commands.length === 0 && <span className="text-sm text-gray-500">No commands declared.</span>}
        </div>
        {msg && <div className="mt-2 text-sm text-gray-600">{msg}</div>}
      </section>

      <section>
        <h2 className="mb-2 font-medium">Run history</h2>
        <RunList runs={runs} />
      </section>

      <section className="max-w-lg">
        <h2 className="mb-2 font-medium">Settings</h2>
        <ScheduleEditor key={device.name + device.last_seen} device={device} onSave={save} />
        <button className="mt-4 text-sm text-red-600 hover:underline" onClick={revoke}>Revoke token</button>
      </section>
    </div>
  )
}
```

In `web/src/App.tsx` import `DeviceDetail` and add the route inside `Shell`:

```tsx
          <Route path="/devices/:name" element={<DeviceDetail state={state} onWake={wake} />} />
```

**Step 3: Verify in the browser**

```bash
make web && go build ./cmd/kumaboard && ./kumaboard serve -config /tmp/kb/config.yaml
```

With the macOS desktop agent running (its config has the `uptime` command), open the device page, click `uptime`, and confirm a run appears as `running` then `ok` with output, without reloading.

**Step 4: Commit**

```bash
git add web/
git commit -m "feat(web): device detail with command buttons, run history, schedule editor"
```

---

## Build step 6: Wake and sleep

### Task 23: `server/wake` magic packets, wake API, headless Windows box config

**Files:**
- Create: `server/wake/wol.go`
- Test: `server/wake/wol_test.go`
- Create: `server/api/wake.go`
- Modify: `server/api/api.go` (remove `mountWake` placeholder)
- Modify: `cmd/kumaboard/serve.go`
- Create: `configs/kuma-agent.windows.example.yaml`
- Create: `deploy/windows/sleep.ps1`

**Step 1: Write the failing test**

`server/wake/wol_test.go`:

```go
package wake

import (
	"bytes"
	"net"
	"testing"
	"time"
)

func TestMagicPacket(t *testing.T) {
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")
	p := MagicPacket(mac)
	if len(p) != 102 || !bytes.Equal(p[:6], bytes.Repeat([]byte{0xff}, 6)) {
		t.Fatalf("bad packet len=%d head=%x", len(p), p[:6])
	}
	for i := 0; i < 16; i++ {
		if !bytes.Equal(p[6+i*6:12+i*6], mac) {
			t.Fatalf("repetition %d wrong", i)
		}
	}
}

func TestSendDeliversThreePackets(t *testing.T) {
	ln, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.LocalAddr().(*net.UDPAddr).Port
	s := &Sender{SourceIP: net.IPv4(127, 0, 0, 1), Broadcast: "127.0.0.1", Port: port}
	if err := s.Send("aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 200)
	for i := 0; i < 3; i++ {
		ln.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, err := ln.ReadFromUDP(buf)
		if err != nil || n != 102 {
			t.Fatalf("packet %d: n=%d err=%v", i, n, err)
		}
	}
}

func TestSendRejectsBadMAC(t *testing.T) {
	s := &Sender{SourceIP: net.IPv4(127, 0, 0, 1), Broadcast: "127.0.0.1", Port: 9}
	if err := s.Send("not-a-mac"); err == nil {
		t.Fatal("bad mac accepted")
	}
}
```

**Step 2: Run to verify failure**

Run: `go test ./server/wake/`
Expected: FAIL, undefined: MagicPacket

**Step 3: Implement**

`server/wake/wol.go`:

```go
// Package wake sends Wake-on-LAN magic packets directly from the server.
package wake

import (
	"errors"
	"fmt"
	"net"
	"time"
)

// Sender emits magic packets from a specific interface to the subnet's
// directed broadcast address. Sending from a specific source IP is what keeps
// Docker and Tailscale interfaces from swallowing the packet.
type Sender struct {
	Interface string // resolved to SourceIP when SourceIP is nil
	SourceIP  net.IP
	Broadcast string
	Port      int // 9 by default
}

// NewSender resolves the interface's first IPv4 address.
func NewSender(iface, broadcast string) (*Sender, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("wake: interface %q: %w", iface, err)
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			return &Sender{Interface: iface, SourceIP: ipn.IP.To4(), Broadcast: broadcast, Port: 9}, nil
		}
	}
	return nil, fmt.Errorf("wake: interface %q has no IPv4 address", iface)
}

// MagicPacket builds the 102-byte payload.
func MagicPacket(mac net.HardwareAddr) []byte {
	p := make([]byte, 0, 102)
	for i := 0; i < 6; i++ {
		p = append(p, 0xff)
	}
	for i := 0; i < 16; i++ {
		p = append(p, mac...)
	}
	return p
}

// Send emits three magic packets 100ms apart.
func (s *Sender) Send(macStr string) error {
	mac, err := net.ParseMAC(macStr)
	if err != nil || len(mac) != 6 {
		return errors.New("wake: invalid MAC address")
	}
	port := s.Port
	if port == 0 {
		port = 9
	}
	bcast := net.ParseIP(s.Broadcast)
	if bcast == nil {
		return errors.New("wake: invalid broadcast address")
	}
	conn, err := net.DialUDP("udp4", &net.UDPAddr{IP: s.SourceIP}, &net.UDPAddr{IP: bcast, Port: port})
	if err != nil {
		return fmt.Errorf("wake: dial: %w", err)
	}
	defer conn.Close()
	packet := MagicPacket(mac)
	for i := 0; i < 3; i++ {
		if _, err := conn.Write(packet); err != nil {
			return fmt.Errorf("wake: send: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}
```

`server/api/wake.go`:

```go
package api

import "net/http"

func (s *server) mountWake(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/devices/{name}/wake", s.wakeDevice)
}

func (s *server) wakeDevice(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	d, err := s.Store.GetDevice(r.Context(), name)
	if err != nil {
		s.notFoundOr500(w, err)
		return
	}
	if d.MAC == "" {
		writeError(w, http.StatusBadRequest, "device has no MAC address configured")
		return
	}
	if s.Wake == nil {
		writeError(w, http.StatusServiceUnavailable, "wake-on-lan is not configured on the server")
		return
	}
	if err := s.Wake(d.MAC); err != nil {
		s.Store.Audit(r.Context(), "admin", "wake", name, "failed", err.Error())
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Store.Audit(r.Context(), "admin", "wake", name, "sent", d.MAC)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent"})
}
```

Remove the `mountWake` placeholder from `server/api/api.go`.

In `cmd/kumaboard/serve.go` `buildHandler`, before creating the API:

```go
	var wakeFn api.WakeFunc
	if cfg.WOL.Interface != "" && cfg.WOL.Broadcast != "" {
		sender, err := wake.NewSender(cfg.WOL.Interface, cfg.WOL.Broadcast)
		if err != nil {
			log.Warn("wake-on-lan disabled", "err", err)
		} else {
			wakeFn = sender.Send
			log.Info("wake-on-lan ready", "source", sender.SourceIP, "broadcast", cfg.WOL.Broadcast)
		}
	}
```

and pass `Wake: wakeFn` in `api.Deps`. For development on the macOS desktop the interface is `en0` (or `en1` for Wi-Fi; check with `ifconfig`), broadcast `192.168.1.255`.

**Step 4: Headless Windows box files**

`deploy/windows/sleep.ps1`:

```powershell
# Puts the machine into S3 sleep with wake events armed (third argument must stay $false).
Add-Type -AssemblyName System.Windows.Forms
[System.Windows.Forms.Application]::SetSuspendState('Suspend', $false, $false) | Out-Null
```

`configs/kuma-agent.windows.example.yaml`:

```yaml
# C:\ProgramData\kuma-agent\config.yaml  (headless Windows box)
device:
  name: windows-headless

server:
  url: wss://192.168.1.150:8443/ws
  ca_file: C:\ProgramData\kuma-agent\ca.pem

token_file: C:\ProgramData\kuma-agent\token

capabilities:
  - metrics
  - custom-commands
  - wol-target

commands:
  sleep:
    description: "Put the machine to sleep"
    run: ["powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "C:\\scripts\\sleep.ps1"]
    timeout_s: 15
    expect_disconnect: true
```

`C:\scripts\` must not be writable by the `kuma-agent` account.

**Step 5: Run tests and try it on the hardware**

Run: `go test ./server/... && make web && go build ./cmd/kumaboard`

Register `windows-headless` in the dashboard with its MAC and `normally_off`. Build `make agent-windows-amd64`, copy the binary, `ca.pem`, config, and token to the headless Windows box, and run `kuma-agent.exe run` from an administrator PowerShell for now (service install is task 25). Click `sleep` on its device page: the run should end `disconnected_as_expected` and the card should turn `offline (expected)`. Click `Wake`: the box should come back and show `online` within two minutes.

**Step 6: Commit**

```bash
git add server/wake/ server/api/ cmd/kumaboard/ configs/ deploy/windows/sleep.ps1
git commit -m "feat(wake): direct magic packets from the LAN interface, wake API, headless Windows box config"
```

---

## Build step 7: Audit view and hardening checks

### Task 24: Audit log view and security verification

**Files:**
- Create: `web/src/views/Audit.tsx`
- Modify: `web/src/App.tsx`

**Step 1: Audit view**

`web/src/views/Audit.tsx`:

```tsx
import { useEffect, useState } from 'react'
import { api, type AuditEntry } from '../api'
import { when } from '../format'

export function Audit() {
  const [entries, setEntries] = useState<AuditEntry[]>([])
  const [done, setDone] = useState(false)

  const load = async (before?: number) => {
    const page = await api<AuditEntry[]>(`/api/audit?limit=100${before ? `&before=${before}` : ''}`)
    setEntries((e) => (before ? [...e, ...page] : page))
    if (page.length < 100) setDone(true)
  }

  useEffect(() => {
    load().catch(() => {})
  }, [])

  return (
    <div>
      <h1 className="mb-4 text-2xl font-semibold">Audit log</h1>
      <table className="w-full text-left text-sm">
        <thead className="border-b text-gray-500">
          <tr><th className="py-1">Time</th><th>Actor</th><th>Action</th><th>Target</th><th>Result</th><th>Detail</th></tr>
        </thead>
        <tbody>
          {entries.map((e) => (
            <tr key={e.id} className="border-b">
              <td className="py-1 whitespace-nowrap">{when(e.ts)}</td>
              <td>{e.actor}</td>
              <td>{e.action}</td>
              <td className="font-mono">{e.target}</td>
              <td className={e.result === 'failed' || e.result === 'auth_failed' ? 'text-red-700' : ''}>{e.result}</td>
              <td className="text-gray-500">{e.detail}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {!done && entries.length > 0 && (
        <button className="mt-3 text-sm text-blue-600 hover:underline" onClick={() => load(entries[entries.length - 1].id)}>Load more</button>
      )}
    </div>
  )
}
```

Add to the `Routes` in `Shell` (`web/src/App.tsx`):

```tsx
          <Route path="/audit" element={<Audit />} />
```

**Step 2: Verify the hardening rules by hand**

With the server running on the macOS desktop's LAN IP (not 127.0.0.1) and the macOS desktop agent connected:

1. Packet capture: `sudo tcpdump -i en0 -A port 8443 | grep -c -i "token"` while an agent reconnects. Expected: `0`. Every byte on 8443 is TLS.
2. `nmap -p 8443 <macos-desktop-ip>` from another LAN machine shows the port open; `curl -k https://<macos-desktop-ip>:8443/api/devices` returns `401` with an empty body.
3. `curl -k -H 'Host: 127.0.0.1:8443' https://<macos-desktop-ip>:8443/api/devices` returns `421` unless `127.0.0.1:8443` is in `listen_addrs`.
4. Six wrong passwords from the login page: the sixth attempt shows the lockout message; the audit view shows five `login failed` rows and one `locked_out`.
5. `curl -k -H 'Origin: https://evil.example' -H 'Cookie: kb_session=<valid>' https://<ip>:8443/api/events` returns `403`.
6. Copy the agent config to a scratch location, set `token_file` to a wrong value, run the agent: the log shows `handshake rejected by server code=auth_failed` and the device page shows the reject reason after the next reconnect.

Record results in `docs/plans/phase-1-acceptance.md` (created in task 28).

**Step 3: Commit**

```bash
git add web/
git commit -m "feat(web): audit log view"
```

---

## Build step 8: Service packaging and deployment

### Task 25: Accounts, service units, setup scripts, Windows service

**Files:**
- Create: `deploy/systemd/kuma-agent.service`
- Create: `deploy/systemd/kumaboard.service`
- Create: `deploy/systemd/setup-agent.sh`
- Create: `deploy/systemd/setup-server.sh`
- Create: `deploy/launchd/com.kumaboard.agent.plist`
- Create: `deploy/launchd/setup-agent.sh`
- Create: `deploy/launchd/sudoers-macos.example`
- Create: `deploy/launchd/local.llm-server.plist.example`
- Create: `deploy/windows/setup.ps1`
- Create: `deploy/windows/README.md`
- Modify: `cmd/kuma-agent/platform_windows.go`
- Modify: `cmd/kuma-agent/platform_unix.go`
- Modify: `cmd/kuma-agent/main.go`
- Create: `configs/kuma-agent.linux.example.yaml`, `configs/kuma-agent.pi.example.yaml`, `configs/kuma-agent.macos.example.yaml`, `configs/kuma-agent.windows.example.yaml`, `configs/kuma-agent.linux.example.yaml`

**Step 1: Linux**

`deploy/systemd/kuma-agent.service`:

```ini
[Unit]
Description=KumaBoard agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=kuma-agent
Group=kuma-agent
ExecStart=/opt/kuma-agent/kuma-agent run -config /etc/kuma-agent/config.yaml
Restart=always
RestartSec=5
# Restart=always also restarts on clean exit, which the phase 2 upgrade path relies on.
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/opt/kuma-agent
PrivateTmp=true
NoNewPrivileges=false
# NoNewPrivileges must stay false: named commands may call sudo -n with exact-match rules.

[Install]
WantedBy=multi-user.target
```

`deploy/systemd/setup-agent.sh`:

```bash
#!/usr/bin/env bash
# Creates the kuma-agent account and install layout on a Debian/Ubuntu host.
# Run as root. Copy the binary, config.yaml, ca.pem, and token afterwards.
set -euo pipefail

id kuma-agent >/dev/null 2>&1 || useradd --system --create-home --home-dir /var/lib/kuma-agent --shell /bin/bash kuma-agent

install -d -m 0755 -o kuma-agent -g kuma-agent /opt/kuma-agent
install -d -m 0755 -o root -g root /etc/kuma-agent
install -m 0644 "$(dirname "$0")/kuma-agent.service" /etc/systemd/system/kuma-agent.service
systemctl daemon-reload
systemctl enable kuma-agent

cat <<EOF
Next:
  cp kuma-agent   /opt/kuma-agent/kuma-agent && chown kuma-agent:kuma-agent /opt/kuma-agent/kuma-agent && chmod 0755 /opt/kuma-agent/kuma-agent
  cp config.yaml  /etc/kuma-agent/config.yaml   (root-owned, 0644)
  cp ca.pem       /etc/kuma-agent/ca.pem        (root-owned, 0644)
  install -m 0600 -o kuma-agent -g kuma-agent token /etc/kuma-agent/token
  systemctl start kuma-agent && journalctl -u kuma-agent -f
EOF
```

`deploy/systemd/kumaboard.service` (server on the control plane host):

```ini
[Unit]
Description=KumaBoard control plane
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=kumaboard
Group=kumaboard
ExecStart=/opt/kumaboard/kumaboard serve -config /etc/kumaboard/config.yaml
Restart=always
RestartSec=5
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/kumaboard
PrivateTmp=true
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
```

`deploy/systemd/setup-server.sh`:

```bash
#!/usr/bin/env bash
# Creates the kumaboard service account and directories on the control plane host. Run as root.
set -euo pipefail

id kumaboard >/dev/null 2>&1 || useradd --system --home-dir /var/lib/kumaboard --shell /usr/sbin/nologin kumaboard
install -d -m 0755 -o root -g root /opt/kumaboard /etc/kumaboard
install -d -m 0750 -o kumaboard -g kumaboard /var/lib/kumaboard
install -m 0644 "$(dirname "$0")/kumaboard.service" /etc/systemd/system/kumaboard.service
systemctl daemon-reload
systemctl enable kumaboard
echo "Copy the kumaboard binary to /opt/kumaboard/ and config to /etc/kumaboard/config.yaml, then: systemctl start kumaboard"
echo "Then: sudo -u kumaboard /opt/kumaboard/kumaboard passwd -config /etc/kumaboard/config.yaml"
```

**Step 2: macOS**

`deploy/launchd/com.kumaboard.agent.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.kumaboard.agent</string>
  <key>ProgramArguments</key>
  <array>
    <string>/opt/kuma-agent/kuma-agent</string>
    <string>run</string>
    <string>-config</string>
    <string>/etc/kuma-agent/config.yaml</string>
  </array>
  <key>UserName</key><string>kuma-agent</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>StandardOutPath</key><string>/var/log/kuma-agent.log</string>
  <key>StandardErrorPath</key><string>/var/log/kuma-agent.log</string>
</dict>
</plist>
```

`deploy/launchd/setup-agent.sh`:

```bash
#!/usr/bin/env bash
# Creates a hidden standard user and the LaunchDaemon on macOS. Run with sudo.
set -euo pipefail

if ! id kuma-agent >/dev/null 2>&1; then
  PW=$(openssl rand -base64 24)
  sysadminctl -addUser kuma-agent -fullName "KumaBoard Agent" -password "$PW" -home /var/kuma-agent -shell /bin/bash
  dscl . create /Users/kuma-agent IsHidden 1
  install -d -o kuma-agent -g staff -m 0750 /var/kuma-agent
  echo "Created user kuma-agent (standard, hidden). Password stored nowhere; use su - from an admin session."
fi

install -d -m 0755 -o kuma-agent -g staff /opt/kuma-agent
install -d -m 0755 -o root -g wheel /etc/kuma-agent
touch /var/log/kuma-agent.log && chown kuma-agent:staff /var/log/kuma-agent.log
install -m 0644 -o root -g wheel "$(dirname "$0")/com.kumaboard.agent.plist" /Library/LaunchDaemons/com.kumaboard.agent.plist

cat <<EOF
Next:
  cp kuma-agent  /opt/kuma-agent/kuma-agent && chown kuma-agent:staff /opt/kuma-agent/kuma-agent && chmod 0755 /opt/kuma-agent/kuma-agent
  cp config.yaml /etc/kuma-agent/config.yaml   (root-owned, 0644)
  cp ca.pem      /etc/kuma-agent/ca.pem        (root-owned, 0644)
  install -m 0600 -o kuma-agent -g staff token /etc/kuma-agent/token
  launchctl bootstrap system /Library/LaunchDaemons/com.kumaboard.agent.plist
  tail -f /var/log/kuma-agent.log
Record whether FileVault is on (fdesetup status): if so, a reboot needs a hands-on unlock.
EOF
```

`deploy/launchd/sudoers-macos.example` (install as `/etc/sudoers.d/kuma-agent`, mode 0440, after `visudo -c -f` passes):

```
kuma-agent ALL=(root) NOPASSWD: /bin/launchctl kickstart -k system/local.llm-server
```

`deploy/launchd/local.llm-server.plist.example`: a LaunchDaemon that runs llm-server as the operator's user with `KeepAlive`, replacing the tmux session. Fill in the llm-server command line and `UserName` for the macOS workstation.

**Step 3: Windows service**

Replace `cmd/kuma-agent/platform_windows.go`:

```go
//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
	"golang.org/x/term"

	"github.com/jhyoong/KumaBoard/agent/config"
)

const (
	platformDefaultConfig = `C:\ProgramData\kuma-agent\config.yaml`
	serviceName           = "kuma-agent"
	logPath               = `C:\ProgramData\kuma-agent\agent.log`
)

// maybeRunService runs as a service when started by the SCM and returns true.
func maybeRunService() bool {
	isSvc, err := svc.IsWindowsService()
	if err != nil || !isSvc {
		return false
	}
	svc.Run(serviceName, &service{cfgPath: defaultConfigPath()})
	return true
}

type service struct{ cfgPath string }

func (s *service) Execute(args []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	cfg, err := config.Load(s.cfgPath)
	if err != nil {
		return true, 1
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return true, 2
	}
	defer logFile.Close()
	log := slog.New(slog.NewTextHandler(logFile, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runWithConfig(ctx, cfg, log)
		close(done)
	}()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for r := range req {
		switch r.Cmd {
		case svc.Interrogate:
			status <- r.CurrentStatus
		case svc.Stop, svc.Shutdown:
			status <- svc.Status{State: svc.StopPending}
			cancel()
			<-done
			return false, 0
		}
	}
	return false, 0
}

func runInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	account := fs.String("account", `.\kuma-agent`, "service logon account")
	password := fs.String("password", "", "account password (prompted when empty)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *password == "" {
		fmt.Fprintf(os.Stderr, "Password for %s: ", *account)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		*password = string(b)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (run as administrator): %w", err)
	}
	defer m.Disconnect()
	if s, err := m.OpenService(serviceName); err == nil {
		s.Close()
		return errors.New("service already installed; run uninstall first")
	}
	// CreateService grants the account the "Log on as a service" right.
	s, err := m.CreateService(serviceName, exe, mgr.Config{
		DisplayName:      "KumaBoard Agent",
		Description:      "Connects this device to the KumaBoard control plane.",
		StartType:        mgr.StartAutomatic,
		ServiceStartName: *account,
		Password:         *password,
	})
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()
	restart := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: 5 * time.Second}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{restart, restart, restart}, 86400); err != nil {
		return fmt.Errorf("set recovery actions: %w", err)
	}
	// Without this, recovery actions fire only on a crash, not on a nonzero exit.
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("set recovery on non-crash failures: %w", err)
	}
	fmt.Println("service installed; start it with: sc start kuma-agent")
	return nil
}

func runUninstall(args []string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return errors.New("service is not installed")
	}
	defer s.Close()
	if st, err := s.Query(); err == nil && st.State != svc.Stopped {
		s.Control(svc.Stop)
		time.Sleep(2 * time.Second)
	}
	if err := s.Delete(); err != nil {
		return err
	}
	fmt.Println("service removed")
	return nil
}
```

The Windows agent exits with a nonzero code when told to stop by the phase 2 upgrader; that is what the recovery settings above are for. In phase 1 a clean stop via `sc stop` exits 0 and is not restarted, which is correct.

In `cmd/kuma-agent/platform_unix.go` add:

```go
func maybeRunService() bool { return false }
```

In `cmd/kuma-agent/main.go`, at the top of `main()` before the argument check:

```go
	if maybeRunService() {
		return
	}
```

Run: `GOOS=windows GOARCH=amd64 go build ./cmd/kuma-agent && go build ./cmd/kuma-agent && go vet ./...`
Expected: both builds succeed. Run `go get golang.org/x/sys@latest` first if the module is missing.

`deploy/windows/setup.ps1`:

```powershell
# Run as Administrator on the Windows box after creating the local standard user kuma-agent.
# Creates the install layout with the intended ACLs, then installs the service.
$ErrorActionPreference = 'Stop'
$base = 'C:\ProgramData\kuma-agent'
New-Item -ItemType Directory -Force -Path "$base\bin" | Out-Null
New-Item -ItemType Directory -Force -Path 'C:\scripts' | Out-Null

# Base directory: admins full, agent read-only.
icacls $base /inheritance:r /grant:r 'Administrators:(OI)(CI)F' 'SYSTEM:(OI)(CI)F' 'kuma-agent:(OI)(CI)RX' | Out-Null
# Binary directory: agent may write (self-upgrade in phase 2).
icacls "$base\bin" /grant:r 'kuma-agent:(OI)(CI)M' | Out-Null
# Scripts directory: agent may read and execute only.
icacls 'C:\scripts' /inheritance:r /grant:r 'Administrators:(OI)(CI)F' 'SYSTEM:(OI)(CI)F' 'kuma-agent:(OI)(CI)RX' | Out-Null

Write-Host "Now copy kuma-agent.exe into $base\bin, config.yaml and ca.pem into $base, sleep.ps1 into C:\scripts, and create $base\token."
Write-Host "Then lock the token down:"
Write-Host "  icacls $base\token /inheritance:r /grant:r 'Administrators:F' 'kuma-agent:R'"
Write-Host "Then install the service:"
Write-Host "  $base\bin\kuma-agent.exe install"
```

`deploy/windows/README.md`:

```markdown
# Windows agent setup

1. Create a local standard user `kuma-agent` with a strong password (Settings, Accounts, Other users, or `net user kuma-agent * /add`). Do not make it an administrator.
2. Deny interactive and Remote Desktop logon for it: open `secpol.msc`, Local Policies, User Rights Assignment, add `kuma-agent` to "Deny log on locally" and "Deny log on through Remote Desktop Services". Do not add it to "Deny log on as a service".
3. Run `setup.ps1` as Administrator. It creates `C:\ProgramData\kuma-agent` with the ACLs and `C:\scripts`.
4. Copy `kuma-agent.exe` to `C:\ProgramData\kuma-agent\bin\`, `config.yaml` and `ca.pem` to `C:\ProgramData\kuma-agent\`, and `sleep.ps1` to `C:\scripts\`. Write the token to `C:\ProgramData\kuma-agent\token` and apply the `icacls` line the script prints.
5. Run `C:\ProgramData\kuma-agent\bin\kuma-agent.exe install` as Administrator. It prompts for the account password, creates the service with automatic start, restart-after-5s recovery, and `FailureActionsOnNonCrashFailures`. The service manager grants "Log on as a service" itself.
6. `sc start kuma-agent`, then check `C:\ProgramData\kuma-agent\agent.log` and the dashboard.
7. Verify `sleep.ps1` works as the agent account by clicking `sleep` in the dashboard. If `SetSuspendState` is refused for a standard user, register a scheduled task that runs `sleep.ps1` as an administrator and change the `sleep` command to `schtasks /Run /TN KumaSleep`.
8. If Windows Defender flags the binary, add an exclusion for `C:\ProgramData\kuma-agent\bin\`.

Never run the service as SYSTEM.
```

**Step 4: Example configs**

Create the remaining example configs in `configs/`, each with the device name, `url: wss://192.168.1.150:8443/ws`, the standard paths, `capabilities: [metrics, custom-commands]`, and one or two safe commands (`uptime` on Unix hosts, `restart-llm-server` on the macOS workstation via the sudoers rule, nothing on the Windows desktop). The Pi config gets no commands and a note that it must not read the SD card for logs. The control plane host config points at `wss://127.0.0.1:8443/ws` only if `127.0.0.1:8443` is in `listen_addrs`; otherwise use the LAN IP.

**Step 5: Deploy by hand to the development fleet**

Follow the setup scripts on the Linux box (systemd), the macOS desktop itself (LaunchDaemon, replacing the foreground agent from task 12), and the headless Windows box (Windows service). Confirm each device shows `online` from a service started with no user logged in, by rebooting each once.

**Step 6: Commit**

```bash
git add deploy/ configs/ cmd/kuma-agent/ go.mod go.sum
git commit -m "feat(deploy): systemd, launchd, Windows service with setup scripts and example configs"
```

---

### Task 26: `make deploy-<device>`

**Files:**
- Create: `deploy/hosts.mk`
- Modify: `Makefile`

**Step 1: hosts.mk**

`deploy/hosts.mk`:

```makefile
# Per-device deploy targets. Format: ssh target, GOOS, GOARCH, restart command.
# Config, ca.pem, and token are never copied by these targets.

DEPLOY_linux-box  := admin@192.168.1.20 linux amd64 'sudo install -o kuma-agent -g kuma-agent -m 0755 /tmp/kuma-agent.new /opt/kuma-agent/kuma-agent && sudo systemctl restart kuma-agent'
DEPLOY_pi          := admin@192.168.1.21 linux arm64 'sudo install -o kuma-agent -g kuma-agent -m 0755 /tmp/kuma-agent.new /opt/kuma-agent/kuma-agent && sudo systemctl restart kuma-agent'
DEPLOY_linux-server      := admin@192.168.1.150 linux amd64 'sudo install -o kuma-agent -g kuma-agent -m 0755 /tmp/kuma-agent.new /opt/kuma-agent/kuma-agent && sudo systemctl restart kuma-agent'
DEPLOY_macos-workstation  := admin@192.168.1.30 darwin arm64 'sudo install -o kuma-agent -g staff -m 0755 /tmp/kuma-agent.new /opt/kuma-agent/kuma-agent && sudo launchctl kickstart -k system/com.kumaboard.agent'
DEPLOY_macos-desktop    := admin@192.168.1.31 darwin arm64 'sudo install -o kuma-agent -g staff -m 0755 /tmp/kuma-agent.new /opt/kuma-agent/kuma-agent && sudo launchctl kickstart -k system/com.kumaboard.agent'
DEPLOY_windows-desktop    := admin@192.168.1.40 windows amd64 'sc stop kuma-agent & timeout /t 3 & move /Y C:\Users\admin\kuma-agent.new C:\ProgramData\kuma-agent\bin\kuma-agent.exe & sc start kuma-agent'
DEPLOY_windows-headless := admin@192.168.1.5 windows amd64 'sc stop kuma-agent & timeout /t 3 & move /Y C:\Users\admin\kuma-agent.new C:\ProgramData\kuma-agent\bin\kuma-agent.exe & sc start kuma-agent'

# Replace the user and IPs above with the real SSH targets. Windows hosts need
# the OpenSSH server feature; scp lands in the user's home there.

deploy-%:
	@test -n "$(DEPLOY_$*)" || (echo "unknown device $*"; exit 1)
	$(eval D_TARGET := $(word 1,$(DEPLOY_$*)))
	$(eval D_GOOS   := $(word 2,$(DEPLOY_$*)))
	$(eval D_GOARCH := $(word 3,$(DEPLOY_$*)))
	$(eval D_RESTART := $(wordlist 4,99,$(DEPLOY_$*)))
	GOOS=$(D_GOOS) GOARCH=$(D_GOARCH) go build -ldflags "$(LDFLAGS)" -o $(BIN)/kuma-agent_$* ./cmd/kuma-agent
	scp $(BIN)/kuma-agent_$* $(D_TARGET):$(if $(filter windows,$(D_GOOS)),kuma-agent.new,/tmp/kuma-agent.new)
	ssh $(D_TARGET) $(D_RESTART)
```

The `-include deploy/hosts.mk` line from task 1 already pulls this in.

**Step 2: Verify**

Run: `make deploy-linux-box VERSION=0.1.0`
Expected: builds, copies, restarts; the dashboard shows the Linux box reconnecting with `agent 0.1.0`.

**Step 3: Commit**

```bash
git add deploy/hosts.mk
git commit -m "build: make deploy-<device> targets"
```

---

## Build step 9: Backup and restore

### Task 27: Nightly backup and restore drill

**Files:**
- Create: `deploy/backup/backup.sh`
- Create: `deploy/backup/kumaboard-backup.service`
- Create: `deploy/backup/kumaboard-backup.timer`
- Create: `deploy/backup/restore.md`

**Step 1: The script**

`deploy/backup/backup.sh`:

```bash
#!/usr/bin/env bash
# Nightly KumaBoard backup. Runs as root from the systemd timer on the control plane host.
# Requires: sqlite3, age, an SSH key for the macOS desktop at /root/.ssh/kumaboard_backup.
set -euo pipefail

DATA=/var/lib/kumaboard
CONF=/etc/kumaboard
RECIPIENT_FILE=$CONF/backup.age.pub      # age public key; private key lives only in the password manager
REMOTE=admin@192.168.1.31                  # macOS desktop
REMOTE_DIR=backups/kumaboard
KEEP=7
STAMP=$(date +%Y%m%d-%H%M%S)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

mkdir -p "$DATA/backup"
sqlite3 "$DATA/kumaboard.db" ".backup '$WORK/kumaboard.db'"
tar -C / -cf "$WORK/kumaboard-$STAMP.tar" \
  -C "$WORK" kumaboard.db \
  -C / var/lib/kumaboard/pki var/lib/kumaboard/releases etc/kumaboard
age -r "$(cat "$RECIPIENT_FILE")" -o "$DATA/backup/kumaboard-$STAMP.tar.age" "$WORK/kumaboard-$STAMP.tar"

scp -i /root/.ssh/kumaboard_backup -o StrictHostKeyChecking=accept-new \
  "$DATA/backup/kumaboard-$STAMP.tar.age" "$REMOTE:$REMOTE_DIR/"

# Retention on both sides.
ls -1t "$DATA/backup"/kumaboard-*.tar.age | tail -n +$((KEEP + 1)) | xargs -r rm -f
ssh -i /root/.ssh/kumaboard_backup "$REMOTE" "ls -1t $REMOTE_DIR/kumaboard-*.tar.age | tail -n +$((KEEP + 1)) | xargs -r rm -f"

echo "kumaboard backup ok: kumaboard-$STAMP.tar.age"
```

`releases/` does not exist until phase 2; create it empty in `setup-server.sh` (`install -d -o kumaboard /var/lib/kumaboard/releases`) so `tar` does not fail.

`deploy/backup/kumaboard-backup.service`:

```ini
[Unit]
Description=KumaBoard nightly backup

[Service]
Type=oneshot
ExecStart=/opt/kumaboard/backup.sh
```

`deploy/backup/kumaboard-backup.timer`:

```ini
[Unit]
Description=KumaBoard nightly backup at 05:30

[Timer]
OnCalendar=*-*-* 05:30:00
Persistent=true

[Install]
WantedBy=timers.target
```

**Step 2: One-time setup (on the control plane host, as root)**

```bash
age-keygen -o /tmp/backup.key            # print the public key line, then store the whole file in the password manager and shred it here
grep 'public key' /tmp/backup.key | cut -d: -f2 | tr -d ' ' > /etc/kumaboard/backup.age.pub
shred -u /tmp/backup.key
ssh-keygen -t ed25519 -N '' -f /root/.ssh/kumaboard_backup
# add /root/.ssh/kumaboard_backup.pub to ~admin/.ssh/authorized_keys on the macOS desktop, and mkdir -p ~/backups/kumaboard there
install -m 0755 deploy/backup/backup.sh /opt/kumaboard/backup.sh
install -m 0644 deploy/backup/kumaboard-backup.service deploy/backup/kumaboard-backup.timer /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now kumaboard-backup.timer
systemctl start kumaboard-backup.service && journalctl -u kumaboard-backup -n 5
```

Also store in the password manager now: the CA private key (`/var/lib/kumaboard/pki/ca.key`) and the age private key.

**Step 3: Restore drill**

`deploy/backup/restore.md`:

```markdown
# Restore

1. Install the server binary on the replacement host and give it the same static IP (192.168.1.150).
2. Run `deploy/systemd/setup-server.sh`.
3. Fetch the newest archive from the macOS desktop and decrypt with the age private key from the password manager:
   `age -d -i backup.key -o kumaboard.tar kumaboard-<stamp>.tar.age`
4. `tar -C / -xf kumaboard.tar` restores `/var/lib/kumaboard/pki`, `/var/lib/kumaboard/releases`, and `/etc/kumaboard`.
   Move the extracted `kumaboard.db` (at the tar root) to `/var/lib/kumaboard/kumaboard.db`.
5. `chown -R kumaboard:kumaboard /var/lib/kumaboard` and `chmod 0600 /var/lib/kumaboard/pki/*.key`.
6. `systemctl start kumaboard`.

Agents reconnect on their own: same IP, same CA, same token hashes. Nothing is reinstalled on any device.

## Drill (do this at phase 1 acceptance)

Restore the newest archive into a scratch directory on the macOS desktop, start a second server from it on another port, and log in:

    mkdir -p /tmp/restore && cd /tmp/restore
    age -d -i backup.key -o kb.tar kumaboard-<stamp>.tar.age && tar -xf kb.tar
    mkdir -p data && mv kumaboard.db data/ && mv var/lib/kumaboard/pki data/
    printf 'listen_addrs: ["127.0.0.1:8444"]\ndata_dir: /tmp/restore/data\n' > config.yaml
    kumaboard serve -config config.yaml

`curl -k https://127.0.0.1:8444/api/login` with the operator password and then `/api/devices` must list every device.

## Moving from the macOS desktop to the control plane host

This is the same procedure, plus: edit each agent's `server.url` to the control plane host IP, and run `kumaboard cert reissue` on the control plane host so the certificate carries the new SAN. The CA does not change, so agents need no new `ca.pem`.
```

**Step 4: Commit**

```bash
git add deploy/backup/
git commit -m "feat(backup): nightly age-encrypted backup to the macOS desktop with restore procedure"
```

---

## Build step 10: Acceptance

### Task 28: Run the phase 1 acceptance criteria

**Files:**
- Create: `docs/plans/phase-1-acceptance.md`

Create the file with one section per criterion below, record the date, the procedure, the observed result, and pass or fail. Fix anything that fails and re-run that criterion. Phase 1 is done when every row is a pass.

| Criterion | Procedure |
|---|---|
| Server restart reconnects all awake agents within 120s | `systemctl restart kumaboard` on the control plane host; watch the devices page; every awake device is `online` within two minutes |
| Every agent machine reboots and reconnects with no user logged in | Reboot each of the seven; do not log in; device shows `online` |
| macOS desktop Wi-Fi drop for 5 minutes, exactly one session | Turn Wi-Fi off for 5 minutes, back on; device returns to `online`; server log shows one `agent connected` and no `replacing duplicate session` loop; `SessionCount` is checked via a temporary log line or by clicking a command once |
| Linux box 01:00 to 05:00 for a week shows `offline_expected` | Set the window with grace 300s; each morning check the audit log and device state history for any `offline_unexpected` transition (add a temporary log line in `registry.reevaluateAll` for the week if needed) |
| Headless Windows box wakes from the dashboard within 120s | With the box asleep, click Wake; time until `online` |
| `sleep` ends as `disconnected_as_expected` | Click `sleep` on the headless Windows box; the run status is checked on its page |
| Killing an agent mid-command records `lost` | Run `uptime`-style long command (`sleep 20` in a test config), `kill -9` the agent process; run shows `lost` |
| No token in cleartext on the LAN | Task 24 step 2 procedure |
| 401 for every `/api` route without a session | `curl` each route listed in design §4 without a cookie; all 401 |
| Unsupported protocol version shows "incompatible" | Build a test agent with `proto.Version` temporarily set to 99 (`-ldflags` cannot change a const; edit, build, revert); deploy to the Linux box; the card shows the incompatible banner; redeploy the real build |
| Agent cannot write its own config | On each device, as the agent account: `touch /etc/kuma-agent/config.yaml` fails (Unix) or `echo x >> C:\ProgramData\kuma-agent\config.yaml` fails (Windows) |
| Restore drill lists all devices | `deploy/backup/restore.md` drill section |

Commit the acceptance record:

```bash
git add docs/plans/phase-1-acceptance.md
git commit -m "docs: phase 1 acceptance record"
```

---

## Deviations from the design to note when done

- `kumaboard device add` CLI exists alongside dashboard registration (task 12).
- The dashboard shell (`index.html` and assets) is served without a session; every `/api` route requires one. The acceptance criterion is read as "every API route".
- The v1 agent does not stream `command_output` chunks; the server accepts them so a later agent can.
- The Windows agent logs to `C:\ProgramData\kuma-agent\agent.log` rather than the Windows event log.
