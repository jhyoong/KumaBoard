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
	if err := s.UpdateDeviceSettings(ctx, "deb", "11:22:33:44:55:66", true, sched, false); err != nil {
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

func TestUpdateDeviceTerminalEnabled(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	s.CreateDevice(ctx, "deb", "", false, Schedule{})

	if err := s.UpdateDeviceSettings(ctx, "deb", "", false, Schedule{}, true); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDevice(ctx, "deb")
	if !d.TerminalEnabled {
		t.Fatal("terminal_enabled not set")
	}

	if err := s.UpdateDeviceSettings(ctx, "deb", "", false, Schedule{}, false); err != nil {
		t.Fatal(err)
	}
	d, _ = s.GetDevice(ctx, "deb")
	if d.TerminalEnabled {
		t.Fatal("terminal_enabled not cleared")
	}
}

func TestTerminalSessionLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _, _ := s.CreateDevice(ctx, "deb", "", false, Schedule{})

	if err := s.InsertTerminalSession(ctx, "sess-1", d.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	n, err := s.CountActiveTerminalSessions(ctx, d.ID)
	if err != nil || n != 1 {
		t.Fatalf("count=%d err=%v, want 1", n, err)
	}

	if err := s.InsertTerminalSession(ctx, "sess-2", d.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	n, _ = s.CountActiveTerminalSessions(ctx, d.ID)
	if n != 2 {
		t.Fatalf("count=%d, want 2", n)
	}

	if err := s.CloseTerminalSession(ctx, "sess-1", 1024, 2048); err != nil {
		t.Fatal(err)
	}
	n, _ = s.CountActiveTerminalSessions(ctx, d.ID)
	if n != 1 {
		t.Fatalf("count=%d after close, want 1", n)
	}
}

func TestEndOpenTerminalSessions(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _, _ := s.CreateDevice(ctx, "dev", "", false, Schedule{})
	s.InsertTerminalSession(ctx, "open-1", d.ID, "admin")
	s.InsertTerminalSession(ctx, "open-2", d.ID, "admin")
	s.InsertTerminalSession(ctx, "done", d.ID, "admin")
	s.CloseTerminalSession(ctx, "done", 1, 2)

	n, err := s.EndOpenTerminalSessions(ctx)
	if err != nil || n != 2 {
		t.Fatalf("closed %d, err %v; want 2", n, err)
	}
	if c, _ := s.CountActiveTerminalSessions(ctx, d.ID); c != 0 {
		t.Fatalf("active = %d after sweep", c)
	}
}

func TestSessionUsername(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	s.UpsertUser(ctx, "operator", "hash")
	uid, _, _ := s.GetUser(ctx, "operator")
	if err := s.CreateSession(ctx, "sid", uid, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if name, err := s.SessionUsername(ctx, "sid"); err != nil || name != "operator" {
		t.Fatalf("got %q, %v", name, err)
	}
	if _, err := s.SessionUsername(ctx, "missing"); err != ErrNotFound {
		t.Fatalf("missing session: %v", err)
	}
}
