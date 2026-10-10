package integration

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/agent/transport"
	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

// messageRecorder wraps an agent handler to capture received envelopes
// and optionally forward them to the inner handler.
type messageRecorder struct {
	inner    transport.Handler
	mu       sync.Mutex
	received []proto.Envelope
	onMsg    func(env *proto.Envelope, send transport.Sender) // optional hook
}

func (r *messageRecorder) OnConnected(ack proto.HelloAck, send transport.Sender) {
	if r.inner != nil {
		r.inner.OnConnected(ack, send)
	}
}

func (r *messageRecorder) OnDisconnected() {
	if r.inner != nil {
		r.inner.OnDisconnected()
	}
}

func (r *messageRecorder) OnMessage(env *proto.Envelope, send transport.Sender) {
	r.mu.Lock()
	r.received = append(r.received, *env)
	r.mu.Unlock()

	if r.onMsg != nil {
		r.onMsg(env, send)
	} else if r.inner != nil {
		r.inner.OnMessage(env, send)
	}
}

func (r *messageRecorder) hasType(typ string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.received {
		if e.Type == typ {
			return true
		}
	}
	return false
}

// helloOverride returns a tweak function that overrides the Hello to report
// a specific agent version, OS, and arch.
func helloOverride(version, os, arch string) func(*transport.Client) {
	return func(c *transport.Client) {
		inner := c.Hello
		c.Hello = func() proto.Hello {
			hl := inner()
			hl.AgentVersion = version
			hl.OS = os
			hl.Arch = arch
			return hl
		}
	}
}

func TestUpgradeTriggerOnHandshake(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	token := h.registerDevice("upgrade-a")
	if err := h.st.IngestRelease(ctx, "0.4.0", "linux", "amd64", "abc123", "sig1", 1000); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetDesiredAgentVersion(ctx, "upgrade-a", "0.4.0"); err != nil {
		t.Fatal(err)
	}

	rec := &messageRecorder{}
	cfg := h.agentConfig("upgrade-a", token)
	h.startAgent(cfg, func(c *transport.Client) {
		helloOverride("0.3.0", "linux", "amd64")(c)
		rec.inner = c.Handler
		c.Handler = rec
	})

	// Wait for the agent to receive an upgrade_request from the hub.
	waitFor(t, 5*time.Second, func() bool {
		return rec.hasType(proto.TypeUpgradeRequest)
	})

	// Verify an upgrade row was created in the store.
	d, err := h.st.GetDevice(ctx, "upgrade-a")
	if err != nil {
		t.Fatal(err)
	}
	u, err := h.st.GetLatestUpgrade(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u == nil {
		t.Fatal("expected upgrade row to exist")
	}
	if u.ToVersion != "0.4.0" || u.FromVersion != "0.3.0" {
		t.Fatalf("upgrade row has wrong versions: from=%s to=%s", u.FromVersion, u.ToVersion)
	}
}

func TestNoUpgradeWhenVersionsMatch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	token := h.registerDevice("match-a")
	if err := h.st.IngestRelease(ctx, "0.4.0", "linux", "amd64", "abc123", "sig1", 1000); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetDesiredAgentVersion(ctx, "match-a", "0.4.0"); err != nil {
		t.Fatal(err)
	}

	rec := &messageRecorder{}
	cfg := h.agentConfig("match-a", token)
	h.startAgent(cfg, func(c *transport.Client) {
		// Agent already at "0.4.0", matching the desired version.
		helloOverride("0.4.0", "linux", "amd64")(c)
		rec.inner = c.Handler
		c.Handler = rec
	})

	// Wait for the agent to connect.
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("match-a") })

	// Give the hub time to run checkUpgradeAfterHandshake.
	time.Sleep(300 * time.Millisecond)

	// Verify no upgrade_request was received.
	if rec.hasType(proto.TypeUpgradeRequest) {
		t.Fatal("received upgrade_request when versions already match")
	}

	// Verify no upgrade row was created.
	d, _ := h.st.GetDevice(ctx, "match-a")
	u, _ := h.st.GetLatestUpgrade(ctx, d.ID)
	if u != nil {
		t.Fatalf("unexpected upgrade row: %+v", u)
	}
}

func TestNoUpgradeWhenAnotherInFlight(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tokenA := h.registerDevice("inflight-a")
	tokenB := h.registerDevice("inflight-b")
	if err := h.st.IngestRelease(ctx, "0.4.0", "linux", "amd64", "abc123", "sig1", 1000); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetDesiredAgentVersion(ctx, "inflight-a", "0.4.0"); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetDesiredAgentVersion(ctx, "inflight-b", "0.4.0"); err != nil {
		t.Fatal(err)
	}

	// Start agent A with a handler that silently absorbs upgrade_request messages
	// so the upgrade row stays in-flight (state "requested") and is not driven
	// to "failed" by the real agent's download attempt.
	recA := &messageRecorder{
		onMsg: func(env *proto.Envelope, send transport.Sender) {
			// Absorb upgrade_request; do nothing so the row stays in-flight.
		},
	}
	cfgA := h.agentConfig("inflight-a", tokenA)
	h.startAgent(cfgA, func(c *transport.Client) {
		helloOverride("0.3.0", "linux", "amd64")(c)
		c.Handler = recA
	})
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("inflight-a") })

	// The handshake should have triggered an upgrade for device A.
	// Wait for it to appear.
	waitFor(t, 3*time.Second, func() bool {
		return recA.hasType(proto.TypeUpgradeRequest)
	})

	// Verify an active (in-flight) upgrade exists.
	active, _ := h.st.GetActiveUpgrade(ctx)
	if active == nil {
		t.Fatal("expected an in-flight upgrade for device A")
	}

	// Now start agent B -- it should not get an upgrade because A is in-flight.
	recB := &messageRecorder{}
	cfgB := h.agentConfig("inflight-b", tokenB)
	h.startAgent(cfgB, func(c *transport.Client) {
		helloOverride("0.3.0", "linux", "amd64")(c)
		recB.inner = c.Handler
		c.Handler = recB
	})
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("inflight-b") })

	// Give the hub time to run checkUpgradeAfterHandshake for device B.
	time.Sleep(300 * time.Millisecond)

	// Verify no upgrade_request was sent to device B.
	if recB.hasType(proto.TypeUpgradeRequest) {
		t.Fatal("received upgrade_request for device B while A is in-flight")
	}

	// Verify no upgrade row was created for device B.
	dB, _ := h.st.GetDevice(ctx, "inflight-b")
	uB, _ := h.st.GetLatestUpgrade(ctx, dB.ID)
	if uB != nil {
		t.Fatalf("unexpected upgrade row for device B: %+v", uB)
	}
}

func TestUpgradeResultUpdatesRow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	token := h.registerDevice("result-a")
	if err := h.st.IngestRelease(ctx, "0.4.0", "linux", "amd64", "abc123", "sig1", 1000); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetDesiredAgentVersion(ctx, "result-a", "0.4.0"); err != nil {
		t.Fatal(err)
	}

	cfg := h.agentConfig("result-a", token)
	h.startAgent(cfg, func(c *transport.Client) {
		helloOverride("0.3.0", "linux", "amd64")(c)

		// Custom handler that responds to upgrade_request with progress results.
		rec := &messageRecorder{
			onMsg: func(env *proto.Envelope, send transport.Sender) {
				if env.Type == proto.TypeUpgradeRequest {
					// Send "downloading" state.
					dlResult := proto.UpgradeResult{
						FromVersion: "0.3.0",
						ToVersion:   "0.4.0",
						State:       proto.UpgradeDownloading,
					}
					dlEnv, _ := proto.New(proto.TypeUpgradeResult, dlResult)
					send.Send(dlEnv)

					// After a brief delay, send "verified" state.
					go func() {
						time.Sleep(100 * time.Millisecond)
						vResult := proto.UpgradeResult{
							FromVersion: "0.3.0",
							ToVersion:   "0.4.0",
							State:       proto.UpgradeVerified,
						}
						vEnv, _ := proto.New(proto.TypeUpgradeResult, vResult)
						send.Send(vEnv)
					}()
				}
			},
		}
		c.Handler = rec
	})

	// Wait for the upgrade row state to reach "verified".
	d, _ := h.st.GetDevice(ctx, "result-a")
	waitFor(t, 5*time.Second, func() bool {
		// Re-fetch because device ID might not be populated yet before handshake.
		d, _ = h.st.GetDevice(ctx, "result-a")
		if d == nil {
			return false
		}
		u, _ := h.st.GetLatestUpgrade(ctx, d.ID)
		return u != nil && u.State == proto.UpgradeVerified
	})

	u, err := h.st.GetLatestUpgrade(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.State != proto.UpgradeVerified {
		t.Fatalf("upgrade state = %q, want %q", u.State, proto.UpgradeVerified)
	}
	if u.FinishedAt == nil {
		t.Fatal("expected finished_at to be set for verified upgrade")
	}
}

func TestRollbackDetection(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	token := h.registerDevice("rollback-a")
	if err := h.st.IngestRelease(ctx, "0.4.0", "linux", "amd64", "abc123", "sig1", 1000); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetDesiredAgentVersion(ctx, "rollback-a", "0.4.0"); err != nil {
		t.Fatal(err)
	}

	// We need the device's OS/arch set so that evaluateUpgrade can find the release.
	// Connect a temporary agent to populate the device row, then disconnect it.
	tmpCfg := h.agentConfig("rollback-a", token)
	tmpAgent := h.startAgent(tmpCfg, func(c *transport.Client) {
		helloOverride("0.3.0", "linux", "amd64")(c)
		// Use a no-op handler that just records but does nothing with upgrade requests.
		c.Handler = &messageRecorder{}
	})
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("rollback-a") })

	// Get the device so we have its ID.
	d, err := h.st.GetDevice(ctx, "rollback-a")
	if err != nil {
		t.Fatal(err)
	}

	// The first handshake offered the upgrade; the no-op agent left it at
	// requested.
	waitFor(t, 3*time.Second, func() bool {
		u, _ := h.st.GetLatestUpgrade(ctx, d.ID)
		return u != nil
	})
	first, err := h.st.GetLatestUpgrade(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	record := func(id, source, state, reason string) {
		t.Helper()
		applied, err := h.st.RecordUpgradeEvent(ctx, store.UpgradeEvent{UpgradeID: id, Source: source, State: state, Reason: reason})
		if err != nil || !applied {
			t.Fatalf("record %s on %s: applied=%v err=%v", state, id, applied, err)
		}
	}
	// connect starts a no-op agent reporting version and returns it once the
	// hub has registered it.
	connect := func(version string) *agentHandle {
		t.Helper()
		a := h.startAgent(h.agentConfig("rollback-a", token), func(c *transport.Client) {
			helloOverride(version, "linux", "amd64")(c)
			c.Handler = &messageRecorder{}
		})
		waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("rollback-a") })
		return a
	}
	disconnect := func(a *agentHandle) {
		t.Helper()
		a.cancel()
		waitFor(t, 3*time.Second, func() bool { return !h.hub.Connected("rollback-a") })
	}
	disconnect(tmpAgent)

	// A reconnect at from_version before the swap is only a reconnect: the
	// binary has not been replaced, so nothing was rolled back.
	record(first.ID, store.UpgradeSourceAgent, proto.UpgradeDownloading, "")
	agent := connect("0.3.0")
	// Give the hub time to run checkUpgradeAfterHandshake.
	time.Sleep(300 * time.Millisecond)
	u, err := h.st.GetUpgrade(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.State != proto.UpgradeDownloading || u.FinishedAt != nil {
		t.Fatalf("reconnect while downloading changed the upgrade: state=%q reason=%q finished=%v",
			u.State, u.FailureReason, u.FinishedAt)
	}
	events, err := h.st.ListUpgradeEvents(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("reconnect while downloading added timeline events: %+v", events)
	}
	disconnect(agent)

	// Close that attempt, then create one in the "restarting" state to
	// simulate a mid-upgrade restart.
	record(first.ID, store.UpgradeSourceServer, proto.UpgradeFailed, "test_cleanup")
	upgradeID, err := h.st.CreateUpgrade(ctx, d.ID, "0.3.0", "0.4.0", "admin")
	if err != nil {
		t.Fatal(err)
	}
	record(upgradeID, store.UpgradeSourceAgent, proto.UpgradeRestarting, "")

	// An agent reporting version "0.3.0" (from_version) after the swap means
	// it rolled back.
	agent = connect("0.3.0")
	waitFor(t, 5*time.Second, func() bool {
		u, _ := h.st.GetLatestUpgrade(ctx, d.ID)
		return u != nil && u.State == proto.UpgradeRolledBack
	})

	u, err = h.st.GetLatestUpgrade(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != upgradeID {
		t.Fatalf("rolled back wrong upgrade: got %s, want %s", u.ID, upgradeID)
	}
	if u.FailureReason != store.UpgradeReasonHandshakeAtFromVersion || u.ClosedBy != store.UpgradeSourceServer {
		t.Fatalf("rolled back with reason=%q closed_by=%q, want %q by %q",
			u.FailureReason, u.ClosedBy, store.UpgradeReasonHandshakeAtFromVersion, store.UpgradeSourceServer)
	}
	disconnect(agent)

	// A handshake at to_version closes an upgrade stuck at restarting: the
	// verified report was lost, but the agent is plainly running the target.
	stuckID, err := h.st.CreateUpgrade(ctx, d.ID, "0.3.0", "0.4.0", "admin")
	if err != nil {
		t.Fatal(err)
	}
	record(stuckID, store.UpgradeSourceAgent, proto.UpgradeRestarting, "")
	connect("0.4.0")
	waitFor(t, 5*time.Second, func() bool {
		u, _ := h.st.GetUpgrade(ctx, stuckID)
		return u != nil && u.State == proto.UpgradeVerified
	})
	u, err = h.st.GetUpgrade(ctx, stuckID)
	if err != nil {
		t.Fatal(err)
	}
	if u.FailureReason != store.UpgradeReasonHandshakeAtToVersion || u.ClosedBy != store.UpgradeSourceServer || u.FinishedAt == nil {
		t.Fatalf("verified with reason=%q closed_by=%q finished=%v, want %q by %q",
			u.FailureReason, u.ClosedBy, u.FinishedAt, store.UpgradeReasonHandshakeAtToVersion, store.UpgradeSourceServer)
	}
	if active, err := h.st.GetActiveUpgrade(ctx); err != nil || active != nil {
		t.Fatalf("an upgrade still holds the slot: %+v (err %v)", active, err)
	}
	// The earlier rollback is history; the inference did not touch it.
	if u, _ := h.st.GetUpgrade(ctx, upgradeID); u == nil || u.State != proto.UpgradeRolledBack {
		t.Fatalf("earlier upgrade changed: %+v", u)
	}
}

func (r *messageRecorder) countType(typ string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.received {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// failingUpgradeAgent starts an agent at 0.3.0 that answers every
// upgrade_request with a selftest_failed result.
func failingUpgradeAgent(h *harness, name, token string, rec *messageRecorder) *agentHandle {
	rec.onMsg = func(env *proto.Envelope, send transport.Sender) {
		if env.Type != proto.TypeUpgradeRequest {
			return
		}
		res, _ := proto.New(proto.TypeUpgradeResult, proto.UpgradeResult{
			FromVersion: "0.3.0", ToVersion: "0.4.0",
			State: proto.UpgradeFailed, Reason: proto.UpgradeReasonSelftestFailed,
		})
		send.Send(res)
	}
	return h.startAgent(h.agentConfig(name, token), func(c *transport.Client) {
		helloOverride("0.3.0", "linux", "amd64")(c)
		c.Handler = rec
	})
}

func (h *harness) postRetry(name string) int {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/api/devices/"+name+"/upgrades/retry", nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(h.session())
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestUpgradeRetryAfterFailureDispatches(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	token := h.registerDevice("retry-a")
	if err := h.st.IngestRelease(ctx, "0.4.0", "linux", "amd64", "abc123", "sig1", 1000); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetDesiredAgentVersion(ctx, "retry-a", "0.4.0"); err != nil {
		t.Fatal(err)
	}

	rec := &messageRecorder{}
	failingUpgradeAgent(h, "retry-a", token, rec)

	d, _ := h.st.GetDevice(ctx, "retry-a")
	waitFor(t, 5*time.Second, func() bool {
		u, _ := h.st.GetLatestUpgrade(ctx, d.ID)
		return u != nil && u.State == proto.UpgradeFailed
	})
	if !h.st.HasFailedUpgrade(ctx, d.ID, "0.4.0") {
		t.Fatal("expected failed upgrade to be recorded")
	}

	if code := h.postRetry("retry-a"); code != http.StatusAccepted {
		t.Fatalf("retry status = %d, want 202", code)
	}
	waitFor(t, 5*time.Second, func() bool { return rec.countType(proto.TypeUpgradeRequest) == 2 })

	ups, err := h.st.GetDeviceUpgrades(ctx, d.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 2 {
		t.Fatalf("got %d upgrade rows, want 2", len(ups))
	}
	var admin int
	for _, u := range ups {
		if u.RequestedBy == "admin" {
			admin++
		}
	}
	if admin != 1 {
		t.Fatalf("got %d admin-requested rows, want 1: %+v", admin, ups)
	}
}

func TestUpgradeAutoOfferBlockedAfterFailure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	token := h.registerDevice("noretry-a")
	if err := h.st.IngestRelease(ctx, "0.4.0", "linux", "amd64", "abc123", "sig1", 1000); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetDesiredAgentVersion(ctx, "noretry-a", "0.4.0"); err != nil {
		t.Fatal(err)
	}

	rec := &messageRecorder{}
	agent := failingUpgradeAgent(h, "noretry-a", token, rec)
	d, _ := h.st.GetDevice(ctx, "noretry-a")
	waitFor(t, 5*time.Second, func() bool {
		u, _ := h.st.GetLatestUpgrade(ctx, d.ID)
		return u != nil && u.State == proto.UpgradeFailed
	})
	agent.cancel()
	waitFor(t, 3*time.Second, func() bool { return !h.hub.Connected("noretry-a") })

	// Retry while disconnected must not claim success.
	if code := h.postRetry("noretry-a"); code != http.StatusConflict {
		t.Fatalf("retry while disconnected status = %d, want 409", code)
	}

	// A fresh handshake must not auto-offer the failed version again.
	rec2 := &messageRecorder{}
	failingUpgradeAgent(h, "noretry-a", token, rec2)
	waitFor(t, 3*time.Second, func() bool { return h.hub.Connected("noretry-a") })
	time.Sleep(300 * time.Millisecond)
	if rec2.hasType(proto.TypeUpgradeRequest) {
		t.Fatal("auto-offered upgrade after a failed attempt without explicit retry")
	}
	ups, _ := h.st.GetDeviceUpgrades(ctx, d.ID, 0)
	if len(ups) != 1 {
		t.Fatalf("got %d upgrade rows, want 1", len(ups))
	}
}
