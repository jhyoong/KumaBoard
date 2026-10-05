package integration

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/agent/config"
	"github.com/jhyoong/KumaBoard/agent/transport"
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
	ag := h.startAgent(cfg, func(c *transport.Client) {
		// Keep the agent connected between heartbeat pings. The default
		// SilenceTimeout in the harness (500ms) is shorter than the
		// HeartbeatInterval (1s), so the agent would reconnect before a
		// ping arrives.  A longer value lets pings keep the session alive.
		c.SilenceTimeout = 10 * time.Second
	})
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
	ag.cancel()
	h.waitStatus(t, id, proto.RunDisconnectedAsExpected, 3*time.Second)

	h2 := newHarness(t)
	_, dev2 := commandAgent(t, h2)
	id2, _ := h2.hub.RequestCommand(context.Background(), dev2, "stay", "test")
	h2.waitStatus(t, id2, proto.RunDispatched, 3*time.Second)
	h2.waitStatus(t, id2, proto.RunNoDisconnect, 10*time.Second)
}

func TestRunLostWhenAgentDies(t *testing.T) {
	h := newHarness(t)
	ag, dev := commandAgent(t, h)
	id, _ := h.hub.RequestCommand(context.Background(), dev, "nap", "test")
	time.Sleep(100 * time.Millisecond)
	ag.cancel()
	h.waitStatus(t, id, proto.RunLost, 3*time.Second)
}
