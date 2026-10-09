package app

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/agent/config"
	"github.com/jhyoong/KumaBoard/agent/transport"
	"github.com/jhyoong/KumaBoard/agent/upgrade"
	"github.com/jhyoong/KumaBoard/proto"
)

const testUpgradeID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

// resultSender records upgrade results and can be made to fail like a dead
// socket. Other message types are accepted and ignored.
type resultSender struct {
	mu      sync.Mutex
	err     error
	results []proto.UpgradeResult
}

func (s *resultSender) Send(env *proto.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if env.Type != proto.TypeUpgradeResult {
		return s.err
	}
	var res proto.UpgradeResult
	if err := env.Unmarshal(&res); err != nil {
		return err
	}
	s.results = append(s.results, res)
	return s.err
}

func (s *resultSender) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *resultSender) take() []proto.UpgradeResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.results
	s.results = nil
	return out
}

// upgradeApp is an app whose upgrader works on a scratch binary directory,
// started in the given post-upgrade state.
func upgradeApp(t *testing.T, startup upgrade.StartupResult) (a *App, dir string) {
	t.Helper()
	dir = t.TempDir()
	bin := filepath.Join(dir, "kuma-agent")
	os.WriteFile(bin, []byte("binary"), 0o755)
	if startup.State != upgrade.StateNormal {
		startup.PendingMarker = filepath.Join(dir, upgrade.PendingFile)
		startup.FromVersion, startup.ToVersion = "0.3.0", "0.4.0"
		err := upgrade.WritePending(startup.PendingMarker, &upgrade.Pending{
			FromVersion: "0.3.0", ToVersion: "0.4.0", StartedAt: time.Now().UTC(),
			RollbackReason: startup.RollbackReason, RollbackDetail: startup.RollbackDetail,
			UpgradeID: startup.UpgradeID,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	a = testAppWithStartup(t, startup)
	a.SetUpgradeBinaryPath(bin)
	t.Cleanup(func() {
		// Never leave a probation timer armed past the test.
		if a.probationTimer != nil {
			a.probationTimer.Stop()
		}
		a.OnDisconnected()
		a.Close(time.Second)
	})
	return a, dir
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestUpgradeSetBinaryPath(t *testing.T) {
	a, dir := upgradeApp(t, upgrade.StartupResult{})
	if a.upgrader.BinaryPath != filepath.Join(dir, "kuma-agent") || a.upgrader.BinaryDir != dir {
		t.Fatalf("upgrader at %q in %q, want the scratch dir %q", a.upgrader.BinaryPath, a.upgrader.BinaryDir, dir)
	}
}

func TestUpgradeConnectFailuresBoundedAtFive(t *testing.T) {
	a, _ := upgradeApp(t, upgrade.StartupResult{State: upgrade.StateProbation, UpgradeID: testUpgradeID})
	for i := 1; i <= 5; i++ {
		a.OnConnectFailed(transport.ConnectStageDial, fmt.Errorf("boom %d", i))
	}
	a.OnConnectFailed(transport.ConnectStageHello, errors.New("EOF sending device-token"))
	a.OnConnectFailed(transport.ConnectStageRejected,
		&transport.HandshakeError{Code: proto.ErrProtocolVersionUnsupported, Message: "handshake rejected"})
	a.OnConnectFailed(transport.ConnectStageRejected, &transport.HandshakeError{HTTPStatus: 401})

	a.mu.Lock()
	kept, total := len(a.connectFailures), a.connectFailureCount
	detail := a.probationDetail()
	a.mu.Unlock()
	if kept != maxConnectFailures || total != 8 {
		t.Fatalf("kept %d of %d failures, want %d of 8", kept, total, maxConnectFailures)
	}

	want := regexp.MustCompile(`^no handshake within 2m0s of starting 0\.4\.0 \(8 connect attempts\)
\d\d:\d\d:\d\d dial: boom 4
\d\d:\d\d:\d\d dial: boom 5
\d\d:\d\d:\d\d hello: EOF sending \[redacted\]
\d\d:\d\d:\d\d rejected: protocol_version_unsupported: handshake rejected
\d\d:\d\d:\d\d rejected: HTTP 401$`)
	if !want.MatchString(detail) {
		t.Fatalf("detail:\n%s", detail)
	}
}

func TestUpgradeProbationDetailWithoutFailures(t *testing.T) {
	a, _ := upgradeApp(t, upgrade.StartupResult{State: upgrade.StateProbation})
	a.mu.Lock()
	detail := a.probationDetail()
	a.mu.Unlock()
	if detail != "no handshake within 2m0s of starting 0.4.0 (0 connect attempts)" {
		t.Fatalf("detail = %q", detail)
	}
}

func TestUpgradeRolledBackCarriesMarkerDetailAndID(t *testing.T) {
	marker := "no handshake within 2m0s of starting 0.4.0 (1 connect attempts)\n12:00:03 dial: refused, token device-token"
	a, _ := upgradeApp(t, upgrade.StartupResult{
		State: upgrade.StateRolledBack, RollbackReason: proto.UpgradeReasonNoHandshake,
		RollbackDetail: marker, UpgradeID: testUpgradeID,
	})
	send := &resultSender{}
	a.OnConnected(proto.HelloAck{}, send)

	got := send.take()
	if len(got) != 1 {
		t.Fatalf("got %d results, want one: %+v", len(got), got)
	}
	want := proto.UpgradeResult{
		FromVersion: "0.3.0", ToVersion: "0.4.0",
		State: proto.UpgradeRolledBack, Reason: proto.UpgradeReasonNoHandshake,
		UpgradeID: testUpgradeID,
		// The marker was written by the other binary; this one still redacts.
		Detail: strings.ReplaceAll(marker, "device-token", "[redacted]"),
	}
	if got[0] != want {
		t.Fatalf("result %+v\nwant   %+v", got[0], want)
	}
	if exists(a.startup.PendingMarker) {
		t.Fatal("marker kept after the report was delivered")
	}

	// Reported once: a reconnect says nothing more.
	a.OnDisconnected()
	a.OnConnected(proto.HelloAck{}, send)
	if again := send.take(); len(again) != 0 {
		t.Fatalf("rolled_back reported again: %+v", again)
	}
}

func TestUpgradeMarkerKeptWhenSendFails(t *testing.T) {
	a, _ := upgradeApp(t, upgrade.StartupResult{
		State: upgrade.StateRolledBack, RollbackReason: proto.UpgradeReasonCrashLoop,
		RollbackDetail: "started 6 times without completing a handshake", UpgradeID: testUpgradeID,
	})
	send := &resultSender{}
	send.fail(errors.New("socket down"))
	a.OnConnected(proto.HelloAck{}, send)
	if got := send.take(); len(got) != 1 {
		t.Fatalf("attempted %d sends, want one", len(got))
	}
	if !exists(a.startup.PendingMarker) {
		t.Fatal("marker deleted although the report was not delivered")
	}
	if a.startup.State != upgrade.StateRolledBack {
		t.Fatalf("startup state reset to %d although the report was not delivered", a.startup.State)
	}

	// The next connect retries, and only then lets go of the marker.
	a.OnDisconnected()
	send.fail(nil)
	a.OnConnected(proto.HelloAck{}, send)
	got := send.take()
	if len(got) != 1 || got[0].State != proto.UpgradeRolledBack || got[0].Reason != proto.UpgradeReasonCrashLoop ||
		got[0].Detail != "started 6 times without completing a handshake" || got[0].UpgradeID != testUpgradeID {
		t.Fatalf("retry sent %+v", got)
	}
	if exists(a.startup.PendingMarker) || a.startup.State != upgrade.StateNormal {
		t.Fatal("marker or state not cleared after delivery")
	}
}

func TestUpgradeVerifiedCarriesIDAndIsRetried(t *testing.T) {
	a, dir := upgradeApp(t, upgrade.StartupResult{State: upgrade.StateProbation, UpgradeID: testUpgradeID})
	old := filepath.Join(dir, "kuma-agent.old")
	os.WriteFile(old, []byte("old-binary"), 0o755)

	send := &resultSender{}
	send.fail(errors.New("socket down"))
	a.OnConnected(proto.HelloAck{}, send)
	// The handshake ends probation whether or not the report got out.
	if exists(a.startup.PendingMarker) || exists(old) {
		t.Fatal("upgrade not confirmed by the handshake")
	}
	if a.startup.State != upgrade.StateProbation {
		t.Fatal("verified dropped although it was not delivered")
	}
	send.take()
	// A timer that fires late must not roll back a confirmed upgrade (it
	// would exit the test binary).
	a.probationExpired()

	a.OnDisconnected()
	send.fail(nil)
	a.OnConnected(proto.HelloAck{}, send)
	got := send.take()
	want := proto.UpgradeResult{FromVersion: "0.3.0", ToVersion: "0.4.0", State: proto.UpgradeVerified, UpgradeID: testUpgradeID}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("sent %+v, want %+v", got, want)
	}
	if a.startup.State != upgrade.StateNormal {
		t.Fatal("state not reset after delivery")
	}
}

func TestUpgradeOutboxFlushedOnConnect(t *testing.T) {
	a, dir := upgradeApp(t, upgrade.StartupResult{})
	kept := proto.UpgradeResult{
		FromVersion: "0.3.0", ToVersion: "0.4.0", State: proto.UpgradeFailed,
		Reason: proto.UpgradeReasonDownloadFailed, UpgradeID: testUpgradeID, Detail: "GET /x: status 404",
	}
	if err := upgrade.WriteOutbox(dir, kept); err != nil {
		t.Fatal(err)
	}
	outbox := filepath.Join(dir, upgrade.OutboxFile)

	send := &resultSender{}
	send.fail(errors.New("socket down"))
	a.OnConnected(proto.HelloAck{}, send)
	if !exists(outbox) {
		t.Fatal("outbox deleted although the send failed")
	}
	send.take()

	a.OnDisconnected()
	send.fail(nil)
	a.OnConnected(proto.HelloAck{}, send)
	if got := send.take(); len(got) != 1 || got[0] != kept {
		t.Fatalf("sent %+v, want %+v", got, kept)
	}
	if exists(outbox) {
		t.Fatal("outbox kept after delivery")
	}

	a.OnDisconnected()
	a.OnConnected(proto.HelloAck{}, send)
	if got := send.take(); len(got) != 0 {
		t.Fatalf("kept result delivered twice: %+v", got)
	}
}

// testAppWithStartup is testApp for an agent that has just been through an
// upgrade restart. It has no capabilities, so connecting starts no metrics.
func testAppWithStartup(t *testing.T, startup upgrade.StartupResult) *App {
	t.Helper()
	cfg := &config.Config{Token: "device-token"}
	cfg.Device.Name = "dev"
	cfg.Server.URL = "wss://127.0.0.1:1/ws"
	return New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), startup)
}
