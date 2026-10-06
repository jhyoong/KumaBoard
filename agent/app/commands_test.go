package app

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/agent/config"
	"github.com/jhyoong/KumaBoard/agent/upgrade"
	"github.com/jhyoong/KumaBoard/proto"
)

// reloadApp builds an app whose config lives in a real file, polled fast.
func reloadApp(t *testing.T, body string) (*App, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin utilities")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmds, err := config.LoadCommands(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Capabilities: []string{}, Token: "device-token", Commands: cmds, Path: path}
	cfg.Device.Name = "dev"
	a := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), upgrade.StartupResult{})
	t.Cleanup(func() { a.Close(time.Second) })
	a.SetConfigPollInterval(20 * time.Millisecond)
	return a, path
}

func nextUpdate(t *testing.T, send fakeSender) proto.CommandsUpdate {
	t.Helper()
	select {
	case env := <-send:
		if env.Type != proto.TypeCommandsUpdate {
			t.Fatalf("message type %q", env.Type)
		}
		var u proto.CommandsUpdate
		if err := env.Unmarshal(&u); err != nil {
			t.Fatal(err)
		}
		return u
	case <-time.After(3 * time.Second):
		t.Fatal("no commands_update")
		return proto.CommandsUpdate{}
	}
}

func noUpdate(t *testing.T, send fakeSender) {
	t.Helper()
	select {
	case env := <-send:
		t.Fatalf("unexpected %s: %s", env.Type, env.Payload)
	case <-time.After(150 * time.Millisecond):
	}
}

const twoCommands = `device: {name: dev}
commands:
  up: {description: Uptime, run: [/usr/bin/uptime], timeout_s: 5}
  wipe: {run: [/bin/true], timeout_s: 5, confirm: true}
`

func TestConfigReloadSendsCommandsUpdate(t *testing.T) {
	a, path := reloadApp(t, twoCommands)
	hello := a.Hello()
	if len(hello.Commands) != 2 || hello.Commands[0].Name != "up" || !hello.Commands[1].Confirm || len(hello.Problems) != 0 {
		t.Fatalf("hello: %+v", hello)
	}
	send := make(fakeSender, 8)
	a.OnConnected(proto.HelloAck{}, send)
	// Nothing changed since hello: no update, however many ticks pass.
	noUpdate(t, send)

	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(twoCommands + "  extra: {run: [/bin/echo, hi], timeout_s: 5}\n")
	u := nextUpdate(t, send)
	if len(u.Commands) != 3 || u.Commands[0].Name != "extra" || u.ConfigError != "" {
		t.Fatalf("after add: %+v", u)
	}

	// A broken file keeps the previous commands and says why.
	write("commands:\n  up: {run: [/usr/bin/uptime\n")
	u = nextUpdate(t, send)
	if len(u.Commands) != 3 || u.ConfigError == "" {
		t.Fatalf("after bad yaml: %+v", u)
	}
	noUpdate(t, send)

	// A valid file with an invalid entry is rejected whole, the same way.
	write("commands:\n  up: {run: [/usr/bin/uptime], timeout_s: 0}\n")
	u = nextUpdate(t, send)
	if len(u.Commands) != 3 || u.ConfigError == "" {
		t.Fatalf("after bad entry: %+v", u)
	}

	write(twoCommands)
	u = nextUpdate(t, send)
	if len(u.Commands) != 2 || u.ConfigError != "" {
		t.Fatalf("after fix: %+v", u)
	}
}

func TestPermissionCheckIsReevaluatedEachPoll(t *testing.T) {
	a, _ := reloadApp(t, twoCommands)
	var failing atomic.Bool
	a.SetFileCheck(func(p string) (string, error) {
		if failing.Load() && p == "/bin/true" {
			return "", os.ErrPermission
		}
		return p, nil
	})
	a.Hello()
	send := make(fakeSender, 8)
	a.OnConnected(proto.HelloAck{}, send)
	noUpdate(t, send)

	// No config edit: the script's permissions went bad on their own.
	failing.Store(true)
	u := nextUpdate(t, send)
	if len(u.Commands) != 1 || u.Commands[0].Name != "up" || len(u.Problems) != 1 || u.Problems[0].Name != "wipe" || u.Problems[0].Reason == "" {
		t.Fatalf("after going bad: %+v", u)
	}
	failing.Store(false)
	u = nextUpdate(t, send)
	if len(u.Commands) != 2 || len(u.Problems) != 0 {
		t.Fatalf("after recovery: %+v", u)
	}
}

func TestReloadWhileDisconnectedIsCarriedByHello(t *testing.T) {
	a, path := reloadApp(t, twoCommands)
	a.Hello()
	if err := os.WriteFile(path, []byte("commands:\n  only: {run: [/bin/echo], timeout_s: 5}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		h := a.Hello()
		if len(h.Commands) == 1 && h.Commands[0].Name == "only" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("hello never picked up the reload: %+v", h.Commands)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Connecting after that hello sends nothing more.
	send := make(fakeSender, 8)
	a.OnConnected(proto.HelloAck{}, send)
	noUpdate(t, send)
}

func TestConfigErrorFollowsHello(t *testing.T) {
	a, path := reloadApp(t, twoCommands)
	if err := os.WriteFile(path, []byte("commands: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // several polls
	if h := a.Hello(); len(h.Commands) != 2 {
		t.Fatalf("previous commands not kept: %+v", h.Commands)
	}
	// hello cannot carry config_error, so it arrives right after connecting.
	send := make(fakeSender, 8)
	a.OnConnected(proto.HelloAck{}, send)
	if u := nextUpdate(t, send); u.ConfigError == "" || len(u.Commands) != 2 {
		t.Fatalf("update: %+v", u)
	}
}

func TestCommandCancelStopsRunByEnvelopeID(t *testing.T) {
	a, _ := reloadApp(t, "commands:\n  nap: {run: [/bin/sleep, \"30\"], timeout_s: 60}\n")
	send := make(fakeSender, 16)
	req, err := proto.New(proto.TypeCommandRequest, proto.CommandRequest{Name: "nap"})
	if err != nil {
		t.Fatal(err)
	}
	a.OnMessage(req, send)
	time.Sleep(200 * time.Millisecond)

	// A cancel for some other run ID is ignored.
	other, _ := proto.New(proto.TypeCommandCancel, nil)
	a.OnMessage(other, send)
	select {
	case env := <-send:
		t.Fatalf("unexpected %s", env.Type)
	case <-time.After(200 * time.Millisecond):
	}

	cancel, _ := proto.New(proto.TypeCommandCancel, nil)
	cancel.ID = req.ID
	a.OnMessage(cancel, send)
	select {
	case env := <-send:
		var res proto.CommandResult
		if err := env.Unmarshal(&res); err != nil {
			t.Fatal(err)
		}
		if env.Type != proto.TypeCommandResult || env.ID != req.ID || res.Status != proto.RunCancelled {
			t.Fatalf("got %s %+v", env.Type, res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run was not cancelled")
	}
}
