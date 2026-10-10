package integration

import (
	"context"
	"sync"
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
	time.Sleep(1 * time.Second)
	h.startServer()
	// The hub registers the session before it emits the connect event, so
	// wait for the event itself.
	waitFor(t, 5*time.Second, func() bool { return h.events.count("connect:a") >= 2 })
	if !h.hub.Connected("a") {
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
