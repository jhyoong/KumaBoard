package hub

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

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

// upgradeDevice registers a device that has handshaken at agentVersion and
// returns it with a session that has no connection behind it: enough for the
// handlers that only read the session's identity.
func upgradeDevice(t *testing.T, st *store.Store, name, agentVersion string) (*store.Device, *Session) {
	t.Helper()
	ctx := context.Background()
	d, _, err := st.CreateDevice(ctx, name, "", false, store.Schedule{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordHandshake(ctx, d.ID, "linux", "amd64", agentVersion, proto.Version, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	d, _ = st.GetDevice(ctx, name)
	return d, &Session{ID: "s-" + name, DeviceID: d.ID, DeviceName: name}
}

// upgradeAt creates an upgrade 0.2.4 -> 0.2.5 for the device and moves it to
// state as the agent would.
func upgradeAt(t *testing.T, st *store.Store, deviceID int64, state string) string {
	t.Helper()
	ctx := context.Background()
	id, err := st.CreateUpgrade(ctx, deviceID, "0.2.4", "0.2.5", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if state != proto.UpgradeRequested {
		if _, err := st.RecordUpgradeEvent(ctx, store.UpgradeEvent{UpgradeID: id, Source: store.UpgradeSourceAgent, State: state}); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func result(t *testing.T, h *Hub, s *Session, res proto.UpgradeResult) {
	t.Helper()
	env, err := proto.New(proto.TypeUpgradeResult, res)
	if err != nil {
		t.Fatal(err)
	}
	h.handleUpgradeResult(context.Background(), s, env)
}

func getUpgrade(t *testing.T, st *store.Store, id string) *store.Upgrade {
	t.Helper()
	u, err := st.GetUpgrade(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func dispatchOf(t *testing.T, st *store.Store, name string) *store.UpgradeDispatch {
	t.Helper()
	d, err := st.GetDevice(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return d.UpgradeDispatch
}

func auditActions(st *store.Store) (actions []string, details string) {
	entries, _ := st.ListAudit(context.Background(), 100, 0)
	for _, e := range entries {
		actions = append(actions, e.Action)
		details += e.Detail + "\n"
	}
	return actions, details
}

func hasAction(actions []string, want string) bool {
	for _, a := range actions {
		if a == want {
			return true
		}
	}
	return false
}

func TestUpgradeResultRecorded(t *testing.T) {
	rec := &recorder{}
	h, st, _ := newTestHub(t, Options{Events: rec})
	ctx := context.Background()
	d, s := upgradeDevice(t, st, "dev", "0.2.4")
	id := upgradeAt(t, st, d.ID, proto.UpgradeRequested)

	result(t, h, s, proto.UpgradeResult{FromVersion: "0.2.4", ToVersion: "0.2.5", State: proto.UpgradeDownloading, UpgradeID: id})
	if u := getUpgrade(t, st, id); u.State != proto.UpgradeDownloading {
		t.Fatalf("state = %q", u.State)
	}
	// The detail is sanitised on the way in and never reaches the audit log.
	result(t, h, s, proto.UpgradeResult{FromVersion: "0.2.4", ToVersion: "0.2.5", State: proto.UpgradeFailed, UpgradeID: id,
		Reason: "Not A Code", Detail: "selftest exited 1\r\n\x1b[31mSECRET-MARKER\x1b[0m"})
	u := getUpgrade(t, st, id)
	if u.State != proto.UpgradeFailed || u.FailureReason != proto.UpgradeReasonInvalid || u.ClosedBy != store.UpgradeSourceAgent ||
		u.FailureDetail != "selftest exited 1\n[31mSECRET-MARKER[0m" {
		t.Fatalf("row = %+v", u)
	}
	events, _ := st.ListUpgradeEvents(ctx, id)
	if len(events) != 3 || events[2].Source != store.UpgradeSourceAgent || events[2].Detail != u.FailureDetail {
		t.Fatalf("events = %+v", events)
	}
	actions, details := auditActions(st)
	if !hasAction(actions, "upgrade_state") || strings.Contains(details, "SECRET-MARKER") {
		t.Fatalf("audit actions=%v details=%q", actions, details)
	}
	if !strings.Contains(details, id+" 0.2.4 -> 0.2.5 "+proto.UpgradeReasonInvalid) {
		t.Fatalf("audit detail missing the upgrade id line: %q", details)
	}
	if rec.count("upgrade:dev") != 2 {
		t.Fatalf("UpgradeChanged fired %d times, want 2", rec.count("upgrade:dev"))
	}
}

func TestUpgradeResultUnknownStateDropped(t *testing.T) {
	rec := &recorder{}
	h, st, _ := newTestHub(t, Options{Events: rec})
	ctx := context.Background()
	d, s := upgradeDevice(t, st, "dev", "0.2.4")
	id := upgradeAt(t, st, d.ID, proto.UpgradeDownloading)

	// An invented state must not be stored: it would hold the fleet slot.
	// requested is the server's alone.
	for _, state := range []string{"holding_the_slot", "", proto.UpgradeRequested} {
		result(t, h, s, proto.UpgradeResult{FromVersion: "0.2.4", ToVersion: "0.2.5", State: state, UpgradeID: id})
	}
	if u := getUpgrade(t, st, id); u.State != proto.UpgradeDownloading {
		t.Fatalf("state = %q", u.State)
	}
	if events, _ := st.ListUpgradeEvents(ctx, id); len(events) != 2 {
		t.Fatalf("dropped results were stored: %+v", events)
	}
	if actions, _ := auditActions(st); hasAction(actions, "upgrade_state") || rec.count("upgrade:dev") != 0 {
		t.Fatalf("dropped results had effects: %v", actions)
	}
}

func TestUpgradeResultUnmatchedChangesNoRow(t *testing.T) {
	rec := &recorder{}
	h, st, _ := newTestHub(t, Options{Events: rec})
	ctx := context.Background()
	d, s := upgradeDevice(t, st, "dev", "0.2.4")
	other, _ := upgradeDevice(t, st, "other", "0.2.4")
	mine := upgradeAt(t, st, d.ID, proto.UpgradeFailed)
	theirs := upgradeAt(t, st, other.ID, proto.UpgradeDownloading)

	for _, res := range []proto.UpgradeResult{
		// Another device's upgrade id.
		{FromVersion: "0.2.4", ToVersion: "0.2.5", State: proto.UpgradeVerified, UpgradeID: theirs},
		// An id that does not exist.
		{FromVersion: "0.2.4", ToVersion: "0.2.5", State: proto.UpgradeVerified, UpgradeID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"},
		// No id, and no upgrade of this device to that version.
		{FromVersion: "0.2.4", ToVersion: "9.9.9", State: proto.UpgradeVerified},
	} {
		result(t, h, s, res)
	}
	if u := getUpgrade(t, st, theirs); u.State != proto.UpgradeDownloading {
		t.Fatalf("another device's row changed: %+v", u)
	}
	if u := getUpgrade(t, st, mine); u.State != proto.UpgradeFailed {
		t.Fatalf("own row changed: %+v", u)
	}
	for _, id := range []string{mine, theirs} {
		if events, _ := st.ListUpgradeEvents(ctx, id); len(events) != 2 {
			t.Fatalf("%s gained events: %+v", id, events)
		}
	}
	actions, _ := auditActions(st)
	if !hasAction(actions, "upgrade_result_unmatched") || hasAction(actions, "upgrade_state") {
		t.Fatalf("audit = %v", actions)
	}
	if rec.count("upgrade:dev")+rec.count("upgrade:other") != 0 {
		t.Fatalf("UpgradeChanged fired for an unmatched result: %v", rec.events)
	}
}

// An agent that does not echo the id still lands on its row by version.
func TestUpgradeResultWithoutIDMatchesByVersion(t *testing.T) {
	h, st, _ := newTestHub(t, Options{})
	d, s := upgradeDevice(t, st, "dev", "0.2.4")
	id := upgradeAt(t, st, d.ID, proto.UpgradeRequested)
	result(t, h, s, proto.UpgradeResult{FromVersion: "0.2.4", ToVersion: "0.2.5", State: proto.UpgradeVerified})
	if u := getUpgrade(t, st, id); u.State != proto.UpgradeVerified {
		t.Fatalf("state = %q", u.State)
	}
}

// A late report after an abandon is kept on the timeline but cannot reopen
// the row; the agent's final word still replaces the abandon.
func TestUpgradeLateResultAfterAbandon(t *testing.T) {
	h, st, _ := newTestHub(t, Options{})
	ctx := context.Background()
	d, s := upgradeDevice(t, st, "dev", "0.2.4")
	id := upgradeAt(t, st, d.ID, proto.UpgradeDownloading)
	if err := st.AbandonUpgrade(ctx, d.ID, id); err != nil {
		t.Fatal(err)
	}
	result(t, h, s, proto.UpgradeResult{ToVersion: "0.2.5", State: proto.UpgradeVerifying, UpgradeID: id})
	if u := getUpgrade(t, st, id); u.State != proto.UpgradeFailed || u.FailureReason != store.UpgradeReasonAbandoned {
		t.Fatalf("row reopened: %+v", u)
	}
	if active, _ := st.GetActiveUpgrade(ctx); active != nil {
		t.Fatalf("slot retaken: %+v", active)
	}
	result(t, h, s, proto.UpgradeResult{ToVersion: "0.2.5", State: proto.UpgradeVerified, UpgradeID: id})
	if u := getUpgrade(t, st, id); u.State != proto.UpgradeVerified || u.ClosedBy != store.UpgradeSourceAgent {
		t.Fatalf("agent report did not supersede: %+v", u)
	}
	events, _ := st.ListUpgradeEvents(ctx, id)
	if len(events) != 5 || events[3].Applied || !events[4].Applied {
		t.Fatalf("events = %+v", events)
	}
}

// One case per row of the handshake table.
func TestUpgradeAfterHandshake(t *testing.T) {
	for _, c := range []struct {
		name         string
		state        string
		agentVersion string
		wantState    string
		wantReason   string
		wantAudit    string
	}{
		{"swapped at from_version", proto.UpgradeSwapped, "0.2.4", proto.UpgradeRolledBack, store.UpgradeReasonHandshakeAtFromVersion, "upgrade_rollback_detected"},
		{"restarting at from_version", proto.UpgradeRestarting, "0.2.4", proto.UpgradeRolledBack, store.UpgradeReasonHandshakeAtFromVersion, "upgrade_rollback_detected"},
		{"restarting at to_version", proto.UpgradeRestarting, "0.2.5", proto.UpgradeVerified, store.UpgradeReasonHandshakeAtToVersion, "upgrade_verify_detected"},
		{"swapped at to_version", proto.UpgradeSwapped, "0.2.5", proto.UpgradeVerified, store.UpgradeReasonHandshakeAtToVersion, "upgrade_verify_detected"},
		{"downloading at to_version", proto.UpgradeDownloading, "0.2.5", proto.UpgradeVerified, store.UpgradeReasonHandshakeAtToVersion, "upgrade_verify_detected"},
		{"requested at from_version", proto.UpgradeRequested, "0.2.4", proto.UpgradeRequested, "", ""},
		{"downloading at from_version", proto.UpgradeDownloading, "0.2.4", proto.UpgradeDownloading, "", ""},
		{"verifying at from_version", proto.UpgradeVerifying, "0.2.4", proto.UpgradeVerifying, "", ""},
		{"selftest at from_version", proto.UpgradeSelftest, "0.2.4", proto.UpgradeSelftest, "", ""},
		{"restarting at another version", proto.UpgradeRestarting, "0.1.0", proto.UpgradeRestarting, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := &recorder{}
			h, st, _ := newTestHub(t, Options{Events: rec})
			ctx := context.Background()
			d, s := upgradeDevice(t, st, "dev", c.agentVersion)
			id := upgradeAt(t, st, d.ID, c.state)
			before, _ := st.ListUpgradeEvents(ctx, id)

			h.checkUpgradeAfterHandshake(ctx, s, c.agentVersion)

			u := getUpgrade(t, st, id)
			if u.State != c.wantState || u.FailureReason != c.wantReason {
				t.Fatalf("row = %s/%q, want %s/%q", u.State, u.FailureReason, c.wantState, c.wantReason)
			}
			events, _ := st.ListUpgradeEvents(ctx, id)
			actions, _ := auditActions(st)
			if c.wantAudit == "" {
				if len(events) != len(before) || rec.count("upgrade:dev") != 0 {
					t.Fatalf("a plain reconnect changed something: events=%+v published=%v", events, rec.events)
				}
				return
			}
			last := events[len(events)-1]
			if u.ClosedBy != store.UpgradeSourceServer || last.Source != store.UpgradeSourceServer || !last.Applied {
				t.Fatalf("row = %+v last event = %+v", u, last)
			}
			if !hasAction(actions, c.wantAudit) || rec.count("upgrade:dev") == 0 {
				t.Fatalf("audit=%v published=%v", actions, rec.events)
			}
			// The agent's own report, if it follows, replaces the inference.
			result(t, h, s, proto.UpgradeResult{ToVersion: "0.2.5", State: proto.UpgradeRolledBack, Reason: proto.UpgradeReasonNoHandshake, UpgradeID: id})
			if u := getUpgrade(t, st, id); u.State != proto.UpgradeRolledBack || u.FailureReason != proto.UpgradeReasonNoHandshake || u.ClosedBy != store.UpgradeSourceAgent {
				t.Fatalf("agent report did not supersede: %+v", u)
			}
		})
	}
}

// The handshake path records why nothing was offered, except when the device
// is only blocked by its own running upgrade.
func TestUpgradeAfterHandshakeRecordsDispatch(t *testing.T) {
	h, st, _ := newTestHub(t, Options{})
	ctx := context.Background()
	d, s := upgradeDevice(t, st, "dev", "0.2.4")
	st.SetDesiredAgentVersion(ctx, "dev", "0.2.5")

	h.checkUpgradeAfterHandshake(ctx, s, "0.2.4")
	if disp := dispatchOf(t, st, "dev"); disp == nil || disp.Code != store.DispatchNoRelease {
		t.Fatalf("no release: dispatch = %+v", disp)
	}

	// Rolled back: the next offer is declined as prev_failed.
	id := upgradeAt(t, st, d.ID, proto.UpgradeRestarting)
	h.checkUpgradeAfterHandshake(ctx, s, "0.2.4")
	if u := getUpgrade(t, st, id); u.State != proto.UpgradeRolledBack {
		t.Fatalf("state = %q", u.State)
	}
	if disp := dispatchOf(t, st, "dev"); disp == nil || disp.Code != store.DispatchPrevFailed {
		t.Fatalf("after rollback: dispatch = %+v", disp)
	}

	// Reconnecting while its own upgrade is still downloading leaves the
	// stored status alone.
	sent := store.UpgradeDispatch{Code: store.DispatchSent, Message: "upgrade request sent", At: time.Now()}
	st.SetUpgradeDispatch(ctx, d.ID, sent)
	upgradeAt(t, st, d.ID, proto.UpgradeDownloading)
	h.checkUpgradeAfterHandshake(ctx, s, "0.2.4")
	if disp := dispatchOf(t, st, "dev"); disp == nil || disp.Code != store.DispatchSent {
		t.Fatalf("own upgrade in flight: dispatch = %+v", disp)
	}

	// At the desired version there is nothing to report.
	st.SetDesiredAgentVersion(ctx, "dev", "0.2.4")
	h.checkUpgradeAfterHandshake(ctx, s, "0.2.4")
	if disp := dispatchOf(t, st, "dev"); disp != nil {
		t.Fatalf("not needed: dispatch = %+v", disp)
	}
}

func TestUpgradeHandshakeRejectedAnnotation(t *testing.T) {
	rec := &recorder{}
	_, st, url := newTestHub(t, Options{Events: rec})
	ctx := context.Background()
	d, token, _ := st.CreateDevice(ctx, "dev", "", false, store.Schedule{})
	id := upgradeAt(t, st, d.ID, proto.UpgradeRestarting)
	before := getUpgrade(t, st, id)

	bad := goodHello("dev", token)
	bad.ProtocolVersion, bad.AgentVersion = 99, "0.2.5"
	reject := func(h proto.Hello) {
		t.Helper()
		conn, resp := dialHello(t, url, h)
		defer conn.CloseNow()
		if c := errCode(t, resp); c != proto.ErrProtocolVersionUnsupported && c != proto.ErrAuthFailed {
			t.Fatalf("got %s", c)
		}
	}
	reject(bad)
	events, _ := st.ListUpgradeEvents(ctx, id)
	want := "agent 0.2.5 speaks protocol 99; server accepts 1-2"
	last := events[len(events)-1]
	if len(events) != 3 || last.Reason != store.UpgradeReasonHandshakeRejected || last.Detail != want ||
		last.Source != store.UpgradeSourceServer || last.State != proto.UpgradeRestarting || !last.Applied {
		t.Fatalf("events = %+v", events)
	}
	// An annotation: the row and its stall clock are untouched.
	if u := getUpgrade(t, st, id); u.State != proto.UpgradeRestarting || !u.UpdatedAt.Equal(before.UpdatedAt) || u.FailureReason != "" {
		t.Fatalf("row changed: %+v", u)
	}
	if rec.count("upgrade:dev") != 1 {
		t.Fatalf("UpgradeChanged fired %d times", rec.count("upgrade:dev"))
	}

	// The agent retries; an identical line is not repeated.
	reject(bad)
	reject(bad)
	if events, _ := st.ListUpgradeEvents(ctx, id); len(events) != 3 {
		t.Fatalf("annotation not deduplicated: %+v", events)
	}
	// A different rejection is a new line.
	bad.ProtocolVersion = 98
	reject(bad)
	if events, _ := st.ListUpgradeEvents(ctx, id); len(events) != 4 {
		t.Fatalf("events = %+v", events)
	}

	// Without a valid token nothing is written, whatever the hello claims.
	bad.Token = "wrong"
	bad.ProtocolVersion = 97
	reject(bad)
	if events, _ := st.ListUpgradeEvents(ctx, id); len(events) != 4 {
		t.Fatalf("unauthenticated hello annotated the upgrade: %+v", events)
	}
}

// Before the swap there is nothing for the annotation to explain.
func TestUpgradeHandshakeRejectedOnlyPastSwap(t *testing.T) {
	_, st, url := newTestHub(t, Options{})
	ctx := context.Background()
	d, token, _ := st.CreateDevice(ctx, "dev", "", false, store.Schedule{})
	id := upgradeAt(t, st, d.ID, proto.UpgradeDownloading)
	bad := goodHello("dev", token)
	bad.ProtocolVersion = 99
	conn, _ := dialHello(t, url, bad)
	conn.CloseNow()
	if events, _ := st.ListUpgradeEvents(ctx, id); len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
}

func TestSweepUpgrades(t *testing.T) {
	for _, c := range []struct {
		state      string
		limit      time.Duration
		timesOut   bool
		wantDetail string
	}{
		{proto.UpgradeRequested, 2 * time.Minute, true, "no report from the agent for 2m while requested"},
		{proto.UpgradeDownloading, 15 * time.Minute, true, "no report from the agent for 15m while downloading"},
		{proto.UpgradeVerifying, 5 * time.Minute, true, "no report from the agent for 5m while verifying"},
		{proto.UpgradeSelftest, 2 * time.Minute, true, "no report from the agent for 2m while selftest"},
		{proto.UpgradeSwapped, 2 * time.Minute, false, ""},
		{proto.UpgradeRestarting, 5 * time.Minute, false, ""},
	} {
		t.Run(c.state, func(t *testing.T) {
			rec := &recorder{}
			h, st, _ := newTestHub(t, Options{Events: rec})
			ctx := context.Background()
			d, _ := upgradeDevice(t, st, "dev", "0.2.4")
			id := upgradeAt(t, st, d.ID, c.state)
			since := getUpgrade(t, st, id).UpdatedAt

			// At the limit nothing happens yet.
			h.SweepUpgrades(ctx, since.Add(c.limit))
			if u := getUpgrade(t, st, id); u.State != c.state || rec.count("upgrade:dev") != 0 {
				t.Fatalf("swept at the limit: %+v published=%v", u, rec.events)
			}

			late := since.Add(c.limit + time.Second)
			h.SweepUpgrades(ctx, late)
			u := getUpgrade(t, st, id)
			if !c.timesOut {
				// Past the swap the row keeps the slot and is only flagged.
				if u.State != c.state || !u.IsStalled(late) || u.IsStalled(since.Add(c.limit)) {
					t.Fatalf("row = %+v", u)
				}
				if active, _ := st.GetActiveUpgrade(ctx); active == nil || active.ID != id {
					t.Fatalf("slot freed: %+v", active)
				}
				if rec.count("upgrade:dev") != 1 {
					t.Fatalf("UpgradeChanged fired %d times, want 1", rec.count("upgrade:dev"))
				}
				// Every later sweep republishes so open pages see the flag.
				h.SweepUpgrades(ctx, late.Add(30*time.Second))
				if rec.count("upgrade:dev") != 2 || getUpgrade(t, st, id).State != c.state {
					t.Fatalf("second sweep: published=%v", rec.events)
				}
				return
			}
			if u.State != proto.UpgradeFailed || u.FailureReason != store.UpgradeReasonTimedOut || u.FailureDetail != c.wantDetail ||
				u.ClosedBy != store.UpgradeSourceServer || u.FinishedAt == nil || !u.FinishedAt.Equal(late) {
				t.Fatalf("row = %+v", u)
			}
			if active, _ := st.GetActiveUpgrade(ctx); active != nil {
				t.Fatalf("slot still held: %+v", active)
			}
			if actions, _ := auditActions(st); !hasAction(actions, "upgrade_timeout") || rec.count("upgrade:dev") != 1 {
				t.Fatalf("audit=%v published=%v", actions, rec.events)
			}
			// A sweep with nothing in flight is a no-op.
			h.SweepUpgrades(ctx, late.Add(time.Hour))
			if rec.count("upgrade:dev") != 1 {
				t.Fatalf("idle sweep published: %v", rec.events)
			}
			// A late result from an agent that was still working wins.
			s := &Session{DeviceID: d.ID, DeviceName: d.Name}
			result(t, h, s, proto.UpgradeResult{ToVersion: "0.2.5", State: proto.UpgradeVerified, UpgradeID: id})
			if u := getUpgrade(t, st, id); u.State != proto.UpgradeVerified || u.ClosedBy != store.UpgradeSourceAgent {
				t.Fatalf("late result did not supersede the timeout: %+v", u)
			}
		})
	}
}

// A timeout frees the slot, so devices that were blocked by it are told.
func TestSweepUpgradesUnblocksWaitingDevices(t *testing.T) {
	h, st, _ := newTestHub(t, Options{})
	ctx := context.Background()
	holder, _ := upgradeDevice(t, st, "holder", "0.2.4")
	blocked, _ := upgradeDevice(t, st, "blocked", "0.2.4")
	id := upgradeAt(t, st, holder.ID, proto.UpgradeDownloading)
	st.SetUpgradeDispatch(ctx, blocked.ID, store.UpgradeDispatch{Code: store.DispatchInFlight, BlockingDevice: "holder", At: time.Now()})
	h.SweepUpgrades(ctx, getUpgrade(t, st, id).UpdatedAt.Add(16*time.Minute))
	if disp := dispatchOf(t, st, "blocked"); disp == nil || disp.Code != store.DispatchWaiting {
		t.Fatalf("dispatch = %+v", disp)
	}
}

func TestInFlightErrorNamesTheDevice(t *testing.T) {
	clock := time.Now()
	h, st, _ := newTestHub(t, Options{now: func() time.Time { return clock }})
	ctx := context.Background()
	holder, _ := upgradeDevice(t, st, "mac-desktop", "0.2.4")
	_, s := upgradeDevice(t, st, "dev", "0.2.4")
	st.SetDesiredAgentVersion(ctx, "dev", "0.2.5")
	id := upgradeAt(t, st, holder.ID, proto.UpgradeRestarting)
	since := getUpgrade(t, st, id).UpdatedAt
	clock = since.Add(12*time.Minute + 30*time.Second)

	d, _ := st.GetDevice(ctx, "dev")
	_, err := h.evaluateUpgrade(ctx, d, s, true)
	if !errors.Is(err, ErrUpgradeInFlight) {
		t.Fatalf("err = %v, want ErrUpgradeInFlight", err)
	}
	var inFlight *InFlightError
	if !errors.As(err, &inFlight) {
		t.Fatalf("err = %T, want *InFlightError", err)
	}
	if inFlight.Device != "mac-desktop" || inFlight.UpgradeID != id || inFlight.State != proto.UpgradeRestarting || !inFlight.Since.Equal(since) {
		t.Fatalf("InFlightError = %+v", inFlight)
	}
	want := "another upgrade is in flight: mac-desktop 0.2.4 -> 0.2.5, restarting for 12m"
	if err.Error() != want {
		t.Fatalf("message = %q, want %q", err.Error(), want)
	}

	// The same facts land on the blocked device's dispatch status.
	h.recordDispatch(ctx, d, "", err)
	disp := dispatchOf(t, st, "dev")
	if disp == nil || disp.Code != store.DispatchInFlight || disp.Message != want || disp.BlockingDevice != "mac-desktop" ||
		disp.BlockingState != proto.UpgradeRestarting || disp.BlockingSince == nil || !disp.BlockingSince.Equal(since) ||
		disp.UpgradeID != "" || !disp.At.Equal(clock) {
		t.Fatalf("dispatch = %+v", disp)
	}
}

func TestRecordDispatchCodes(t *testing.T) {
	rec := &recorder{}
	h, st, _ := newTestHub(t, Options{Events: rec})
	ctx := context.Background()
	d, _ := upgradeDevice(t, st, "dev", "0.2.4")
	for _, c := range []struct {
		err  error
		id   string
		code string
	}{
		{nil, "01ARZ3NDEKTSV4RRFFQ69G5FAV", store.DispatchSent},
		{ErrUpgradePrevFailed, "", store.DispatchPrevFailed},
		{ErrUpgradeNoRelease, "", store.DispatchNoRelease},
		{ErrUpgradeNotConnected, "", store.DispatchNotConnected},
		{&InFlightError{Device: "other", State: proto.UpgradeSwapped, Since: time.Now()}, "", store.DispatchInFlight},
		{errors.New("send upgrade request: broken pipe"), "", store.DispatchSendFailed},
	} {
		h.recordDispatch(ctx, d, c.id, c.err)
		disp := dispatchOf(t, st, "dev")
		if disp == nil || disp.Code != c.code || disp.UpgradeID != c.id || disp.Message == "" || disp.At.IsZero() {
			t.Fatalf("%v: dispatch = %+v, want code %s", c.err, disp, c.code)
		}
		if (disp.BlockingDevice != "") != (c.code == store.DispatchInFlight) || (disp.BlockingSince != nil) != (c.code == store.DispatchInFlight) {
			t.Fatalf("%s: blocking fields = %+v", c.code, disp)
		}
	}
	// Not needed clears whatever was stored.
	n := rec.count("upgrade:dev")
	d, _ = st.GetDevice(ctx, "dev")
	h.recordDispatch(ctx, d, "", ErrUpgradeNotNeeded)
	if disp := dispatchOf(t, st, "dev"); disp != nil || rec.count("upgrade:dev") != n+1 {
		t.Fatalf("not cleared: %+v", disp)
	}
	// With nothing stored there is nothing to publish.
	d, _ = st.GetDevice(ctx, "dev")
	h.recordDispatch(ctx, d, "", ErrUpgradeNotNeeded)
	if rec.count("upgrade:dev") != n+1 {
		t.Fatalf("clearing nothing published: %v", rec.events)
	}
}

func TestRefreshBlocked(t *testing.T) {
	rec := &recorder{}
	h, st, _ := newTestHub(t, Options{Events: rec})
	ctx := context.Background()
	holder, hs := upgradeDevice(t, st, "holder", "0.2.4")
	blocked, _ := upgradeDevice(t, st, "blocked", "0.2.4")
	offline, _ := upgradeDevice(t, st, "offline", "0.2.4")
	id := upgradeAt(t, st, holder.ID, proto.UpgradeDownloading)
	since := time.Now()
	st.SetUpgradeDispatch(ctx, holder.ID, store.UpgradeDispatch{Code: store.DispatchSent, UpgradeID: id, At: since})
	st.SetUpgradeDispatch(ctx, blocked.ID, store.UpgradeDispatch{Code: store.DispatchInFlight, Message: "another upgrade is in flight",
		BlockingDevice: "holder", BlockingState: proto.UpgradeDownloading, BlockingSince: &since, At: since})
	st.SetUpgradeDispatch(ctx, offline.ID, store.UpgradeDispatch{Code: store.DispatchNotConnected, At: since})

	// A non-terminal report frees nothing.
	result(t, h, hs, proto.UpgradeResult{ToVersion: "0.2.5", State: proto.UpgradeVerifying, UpgradeID: id})
	if disp := dispatchOf(t, st, "blocked"); disp.Code != store.DispatchInFlight {
		t.Fatalf("unblocked early: %+v", disp)
	}

	result(t, h, hs, proto.UpgradeResult{ToVersion: "0.2.5", State: proto.UpgradeFailed, Reason: proto.UpgradeReasonSelftestFailed, UpgradeID: id})
	disp := dispatchOf(t, st, "blocked")
	if disp == nil || disp.Code != store.DispatchWaiting || disp.Message == "" || disp.BlockingDevice != "" ||
		disp.BlockingState != "" || disp.BlockingSince != nil {
		t.Fatalf("blocked device = %+v", disp)
	}
	if rec.count("upgrade:blocked") != 1 {
		t.Fatalf("UpgradeChanged for the blocked device fired %d times", rec.count("upgrade:blocked"))
	}
	// Other codes are left as they were, and nothing is re-offered.
	if disp := dispatchOf(t, st, "holder"); disp.Code != store.DispatchSent {
		t.Fatalf("holder = %+v", disp)
	}
	if disp := dispatchOf(t, st, "offline"); disp.Code != store.DispatchNotConnected {
		t.Fatalf("offline = %+v", disp)
	}
	if ups, _ := st.GetDeviceUpgrades(ctx, blocked.ID, 0); len(ups) != 0 {
		t.Fatalf("blocked device was re-offered: %+v", ups)
	}
}

// readUpgradeRequest reads from an agent connection until an upgrade request
// arrives.
func readUpgradeRequest(t *testing.T, conn *websocket.Conn) proto.UpgradeRequest {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("no upgrade request: %v", err)
		}
		env, err := proto.Decode(data)
		if err != nil {
			t.Fatal(err)
		}
		if env.Type != proto.TypeUpgradeRequest {
			continue
		}
		var req proto.UpgradeRequest
		if err := env.Unmarshal(&req); err != nil {
			t.Fatal(err)
		}
		return req
	}
}

// End to end over a real socket: the request carries the row id, the second
// device is told who holds the slot, and is moved to waiting when it frees.
func TestUpgradeDispatchOverSocket(t *testing.T) {
	h, st, url := newTestHub(t, Options{})
	ctx := context.Background()
	_, tokenA, _ := st.CreateDevice(ctx, "dev-a", "", false, store.Schedule{})
	_, tokenB, _ := st.CreateDevice(ctx, "dev-b", "", false, store.Schedule{})
	st.IngestRelease(ctx, "0.2.0", "linux", "amd64", "abc", "sig", 100)
	st.SetDesiredAgentVersion(ctx, "dev-a", "0.2.0")
	st.SetDesiredAgentVersion(ctx, "dev-b", "0.2.0")

	// dev-a is offered the upgrade by its handshake.
	connA, resp := dialHello(t, url, goodHello("dev-a", tokenA))
	defer connA.CloseNow()
	if resp.Type != proto.TypeHelloAck {
		t.Fatalf("handshake: %s", resp.Type)
	}
	req := readUpgradeRequest(t, connA)
	dA, _ := st.GetDevice(ctx, "dev-a")
	u, _ := st.GetLatestUpgrade(ctx, dA.ID)
	if u == nil || req.UpgradeID != u.ID || req.Version != "0.2.0" || u.RequestedBy != "server" {
		t.Fatalf("request = %+v row = %+v", req, u)
	}
	waitFor(t, func() bool { d := dispatchOf(t, st, "dev-a"); return d != nil && d.Code == store.DispatchSent })
	if disp := dispatchOf(t, st, "dev-a"); disp.UpgradeID != u.ID {
		t.Fatalf("dispatch = %+v", disp)
	}

	// dev-b connects while dev-a holds the slot.
	connB, _ := dialHello(t, url, goodHello("dev-b", tokenB))
	defer connB.CloseNow()
	waitFor(t, func() bool { d := dispatchOf(t, st, "dev-b"); return d != nil && d.Code == store.DispatchInFlight })
	if disp := dispatchOf(t, st, "dev-b"); disp.BlockingDevice != "dev-a" || disp.BlockingState != proto.UpgradeRequested {
		t.Fatalf("dispatch = %+v", disp)
	}
	// An explicit retry is refused the same way and names the holder.
	dB, _ := st.GetDevice(ctx, "dev-b")
	_, err := h.RetryUpgrade(ctx, dB)
	var inFlight *InFlightError
	if !errors.As(err, &inFlight) || inFlight.Device != "dev-a" || inFlight.UpgradeID != u.ID {
		t.Fatalf("retry err = %v", err)
	}

	// dev-a reports failure over its socket; dev-b reads waiting.
	env, _ := proto.New(proto.TypeUpgradeResult, proto.UpgradeResult{FromVersion: "0.1.0", ToVersion: "0.2.0",
		State: proto.UpgradeFailed, Reason: proto.UpgradeReasonDownloadFailed, Detail: "GET /x: status 404", UpgradeID: req.UpgradeID})
	b, _ := proto.Encode(env)
	if err := connA.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { d := dispatchOf(t, st, "dev-b"); return d != nil && d.Code == store.DispatchWaiting })
	if u := getUpgrade(t, st, u.ID); u.State != proto.UpgradeFailed || u.FailureDetail != "GET /x: status 404" {
		t.Fatalf("row = %+v", u)
	}

	// The operator's retry now goes out, with its own id.
	id, err := h.RetryUpgrade(ctx, dB)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if req := readUpgradeRequest(t, connB); req.UpgradeID != id || id == u.ID {
		t.Fatalf("request id = %q, retry returned %q", req.UpgradeID, id)
	}
	if disp := dispatchOf(t, st, "dev-b"); disp.Code != store.DispatchSent || disp.UpgradeID != id {
		t.Fatalf("dispatch = %+v", disp)
	}
}

// An offline device reads not_connected; clearing its target clears that.
func TestSendUpgradeOffline(t *testing.T) {
	h, st, _ := newTestHub(t, Options{})
	ctx := context.Background()
	upgradeDevice(t, st, "dev", "0.2.4")
	st.SetDesiredAgentVersion(ctx, "dev", "0.2.5")
	d, _ := st.GetDevice(ctx, "dev")
	if _, err := h.SendUpgradeToDevice(ctx, d); !errors.Is(err, ErrUpgradeNotConnected) {
		t.Fatalf("err = %v", err)
	}
	if disp := dispatchOf(t, st, "dev"); disp == nil || disp.Code != store.DispatchNotConnected {
		t.Fatalf("dispatch = %+v", disp)
	}
	st.SetDesiredAgentVersion(ctx, "dev", "")
	d, _ = st.GetDevice(ctx, "dev")
	if _, err := h.SendUpgradeToDevice(ctx, d); !errors.Is(err, ErrUpgradeNotNeeded) {
		t.Fatalf("err = %v", err)
	}
	if disp := dispatchOf(t, st, "dev"); disp != nil {
		t.Fatalf("dispatch not cleared: %+v", disp)
	}
}

// Concurrent evaluations (a PATCH racing a handshake offer, or two devices
// handshaking together) must not both take the fleet's one slot.
func TestReserveUpgradeIsExclusive(t *testing.T) {
	h, st, _ := newTestHub(t, Options{})
	ctx := context.Background()
	st.IngestRelease(ctx, "0.2.5", "linux", "amd64", "abc", "sig", 100)
	const n = 8
	devices := make([]*store.Device, n)
	for i := range devices {
		name := "dev" + string(rune('a'+i))
		upgradeDevice(t, st, name, "0.2.4")
		st.SetDesiredAgentVersion(ctx, name, "0.2.5")
		devices[i], _ = st.GetDevice(ctx, name)
	}
	errs := make(chan error, n)
	start := make(chan struct{})
	for _, d := range devices {
		go func() {
			<-start
			_, _, err := h.reserveUpgrade(ctx, d, false)
			errs <- err
		}()
	}
	close(start)
	won := 0
	for range devices {
		if err := <-errs; err == nil {
			won++
		} else if !errors.Is(err, ErrUpgradeInFlight) {
			t.Fatalf("err = %v, want ErrUpgradeInFlight", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d evaluations took the slot, want 1", won)
	}
}

// A PATCH that lands after the handshake offer already started this device's
// upgrade to the same target must not relabel it as blocked by itself.
func TestSendUpgradeKeepsOwnInFlightDispatch(t *testing.T) {
	h, st, _ := newTestHub(t, Options{})
	ctx := context.Background()
	d, s := upgradeDevice(t, st, "dev", "0.2.4")
	st.SetDesiredAgentVersion(ctx, "dev", "0.2.5")
	h.mu.Lock()
	h.sessions["dev"] = s
	h.mu.Unlock()
	id := upgradeAt(t, st, d.ID, proto.UpgradeDownloading)
	d, _ = st.GetDevice(ctx, "dev")
	h.recordDispatch(ctx, d, id, nil)

	d, _ = st.GetDevice(ctx, "dev")
	if _, err := h.SendUpgradeToDevice(ctx, d); !errors.Is(err, ErrUpgradeInFlight) {
		t.Fatalf("err = %v, want ErrUpgradeInFlight", err)
	}
	if disp := dispatchOf(t, st, "dev"); disp == nil || disp.Code != store.DispatchSent || disp.UpgradeID != id {
		t.Fatalf("dispatch = %+v, want sent for %s", disp, id)
	}

	// A different target while its own upgrade runs is a real refusal.
	st.SetDesiredAgentVersion(ctx, "dev", "0.2.6")
	d, _ = st.GetDevice(ctx, "dev")
	h.SendUpgradeToDevice(ctx, d)
	if disp := dispatchOf(t, st, "dev"); disp == nil || disp.Code != store.DispatchInFlight || disp.BlockingDevice != "dev" {
		t.Fatalf("dispatch = %+v, want in_flight naming dev", disp)
	}
}
