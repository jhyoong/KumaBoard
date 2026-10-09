package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/agent/app"
	"github.com/jhyoong/KumaBoard/agent/transport"
	"github.com/jhyoong/KumaBoard/agent/upgrade"
	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

// The dashboard API's upgrade JSON, decoded by field name so a renamed field
// fails these tests.

type apiUpgradeEvent struct {
	TS      time.Time `json:"ts"`
	Source  string    `json:"source"`
	State   string    `json:"state"`
	Reason  string    `json:"reason"`
	Detail  string    `json:"detail"`
	Applied bool      `json:"applied"`
}

type apiUpgrade struct {
	ID            string            `json:"id"`
	DeviceID      int64             `json:"device_id"`
	FromVersion   string            `json:"from_version"`
	ToVersion     string            `json:"to_version"`
	RequestedBy   string            `json:"requested_by"`
	StartedAt     time.Time         `json:"started_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	FinishedAt    *time.Time        `json:"finished_at"`
	State         string            `json:"state"`
	FailureReason string            `json:"failure_reason"`
	FailureDetail string            `json:"failure_detail"`
	ClosedBy      string            `json:"closed_by"`
	Stalled       bool              `json:"stalled"`
	Events        []apiUpgradeEvent `json:"events"`
}

type apiDispatch struct {
	Code           string     `json:"code"`
	Message        string     `json:"message"`
	BlockingDevice string     `json:"blocking_device"`
	UpgradeID      string     `json:"upgrade_id"`
	At             time.Time  `json:"at"`
	BlockingState  string     `json:"blocking_state"`
	BlockingSince  *time.Time `json:"blocking_since"`
}

type apiLatestUpgrade struct {
	ID            string    `json:"id"`
	FromVersion   string    `json:"from_version"`
	ToVersion     string    `json:"to_version"`
	State         string    `json:"state"`
	FailureReason string    `json:"failure_reason"`
	UpdatedAt     time.Time `json:"updated_at"`
	Stalled       bool      `json:"stalled"`
}

type apiDevice struct {
	Name            string            `json:"name"`
	UpgradeDispatch *apiDispatch      `json:"upgrade_dispatch"`
	LatestUpgrade   *apiLatestUpgrade `json:"latest_upgrade"`
}

// api calls the dashboard API as the logged-in operator. A non-nil out
// receives the decoded response body.
func (h *harness) api(method, path string, body, out any) int {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	req.AddCookie(h.session())
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			h.t.Fatalf("%s %s: status %d, body %q: %v", method, path, resp.StatusCode, raw, err)
		}
	}
	return resp.StatusCode
}

// patchTarget sets a device's desired agent version through the API and
// returns the device summary from the response.
func (h *harness) patchTarget(name, version string) apiDevice {
	h.t.Helper()
	var d apiDevice
	if code := h.api(http.MethodPatch, "/api/devices/"+name, map[string]string{"desired_agent_version": version}, &d); code != http.StatusOK {
		h.t.Fatalf("PATCH %s: status %d", name, code)
	}
	if d.Name != name {
		h.t.Fatalf("PATCH %s returned the summary of %q", name, d.Name)
	}
	return d
}

// apiDevice returns the device's summary from GET /api/devices.
func (h *harness) apiDevice(name string) apiDevice {
	h.t.Helper()
	var all []apiDevice
	if code := h.api(http.MethodGet, "/api/devices", nil, &all); code != http.StatusOK {
		h.t.Fatalf("GET /api/devices: status %d", code)
	}
	for _, d := range all {
		if d.Name == name {
			return d
		}
	}
	h.t.Fatalf("device %q not in GET /api/devices", name)
	return apiDevice{}
}

func (h *harness) dispatchCode(name string) string {
	h.t.Helper()
	if d := h.apiDevice(name).UpgradeDispatch; d != nil {
		return d.Code
	}
	return ""
}

// apiUpgrade returns one upgrade with its timeline from the detail endpoint.
func (h *harness) apiUpgrade(name, id string) apiUpgrade {
	h.t.Helper()
	var u apiUpgrade
	if code := h.api(http.MethodGet, "/api/devices/"+name+"/upgrades/"+id, nil, &u); code != http.StatusOK {
		h.t.Fatalf("GET upgrade %s of %s: status %d", id, name, code)
	}
	return u
}

func eventStates(events []apiUpgradeEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.State
	}
	return out
}

func assertEventStates(t *testing.T, events []apiUpgradeEvent, want ...string) {
	t.Helper()
	got := eventStates(events)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("timeline = %v, want %v", got, want)
	}
	for i := 1; i < len(events); i++ {
		if events[i].TS.Before(events[i-1].TS) {
			t.Fatalf("timeline goes backwards at %s: %s then %s", events[i].State, events[i-1].TS, events[i].TS)
		}
	}
}

// upgradeProbe is a scripted agent: it keeps the upgrade requests it is sent
// and reports only what the test tells it to.
type upgradeProbe struct {
	mu   sync.Mutex
	send transport.Sender
	reqs []proto.UpgradeRequest
}

func (p *upgradeProbe) OnConnected(_ proto.HelloAck, send transport.Sender) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.send = send
}

func (p *upgradeProbe) OnDisconnected() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.send = nil
}

func (p *upgradeProbe) OnMessage(env *proto.Envelope, _ transport.Sender) {
	if env.Type != proto.TypeUpgradeRequest {
		return
	}
	var req proto.UpgradeRequest
	if err := env.Unmarshal(&req); err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, req)
}

func (p *upgradeProbe) requests() []proto.UpgradeRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]proto.UpgradeRequest(nil), p.reqs...)
}

// waitRequest returns the n-th (1-based) upgrade request the probe received.
func (p *upgradeProbe) waitRequest(t *testing.T, n int) proto.UpgradeRequest {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool { return len(p.requests()) >= n })
	return p.requests()[n-1]
}

// report sends payload as an upgrade_result.
func (p *upgradeProbe) report(t *testing.T, payload any) {
	t.Helper()
	p.mu.Lock()
	send := p.send
	p.mu.Unlock()
	if send == nil {
		t.Fatal("probe is not connected")
	}
	env, err := proto.New(proto.TypeUpgradeResult, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := send.Send(env); err != nil {
		t.Fatal(err)
	}
}

// startProbe connects a scripted agent at 0.3.0 linux/amd64. It stays on one
// connection: a probe sends no metrics, and with the harness's short silence
// timeout it would otherwise reconnect twice a second, each handshake being a
// fresh upgrade offer.
func (h *harness) startProbe(name, token string) (*upgradeProbe, *agentHandle) {
	h.t.Helper()
	p := &upgradeProbe{}
	a := h.startAgent(h.agentConfig(name, token), func(c *transport.Client) {
		helloOverride("0.3.0", "linux", "amd64")(c)
		c.Handler = p
		c.SilenceTimeout = time.Minute
	})
	waitFor(h.t, 3*time.Second, func() bool { return h.hub.Connected(name) })
	return p, a
}

// noReleaseVersion is a version no test ingests.
const noReleaseVersion = "0.0.1"

// startSettledProbe connects a scripted agent and returns once the hub has
// finished its post-handshake upgrade evaluation for it, so a PATCH that
// follows cannot race that evaluation. The evaluation runs in the background
// and leaves a trace only when it has something to say, so the device is
// first given a target with no release: offline that reads not_connected, and
// the handshake turns it into no_release. No upgrade may be in flight.
func (h *harness) startSettledProbe(name, token string) (*upgradeProbe, *agentHandle) {
	h.t.Helper()
	d := h.patchTarget(name, noReleaseVersion)
	if d.UpgradeDispatch == nil || d.UpgradeDispatch.Code != store.DispatchNotConnected {
		h.t.Fatalf("offline PATCH dispatch = %+v, want %s", d.UpgradeDispatch, store.DispatchNotConnected)
	}
	p, a := h.startProbe(name, token)
	waitFor(h.t, 5*time.Second, func() bool { return h.dispatchCode(name) == store.DispatchNoRelease })
	return p, a
}

func (h *harness) deviceID(name string) int64 {
	h.t.Helper()
	d, err := h.st.GetDevice(context.Background(), name)
	if err != nil {
		h.t.Fatal(err)
	}
	return d.ID
}

func (h *harness) upgradeRow(id string) *store.Upgrade {
	h.t.Helper()
	u, err := h.st.GetUpgrade(context.Background(), id)
	if err != nil {
		h.t.Fatalf("get upgrade %s: %v", id, err)
	}
	return u
}

func (h *harness) upgradeEvents(id string) []store.UpgradeEvent {
	h.t.Helper()
	ev, err := h.st.ListUpgradeEvents(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return ev
}

func (h *harness) assertSlotFree() {
	h.t.Helper()
	if active, err := h.st.GetActiveUpgrade(context.Background()); err != nil || active != nil {
		h.t.Fatalf("an upgrade holds the fleet slot: %+v (err %v)", active, err)
	}
}

// realUpgradeAgent builds a real agent whose upgrader works in a scratch
// directory, so an upgrade that reached the swap could not replace the test
// binary. It returns the app and the stand-in binary path.
func (h *harness) realUpgradeAgent(name, token string) (*app.App, string) {
	h.t.Helper()
	bin := filepath.Join(h.t.TempDir(), "kuma-agent")
	if err := os.WriteFile(bin, []byte("stand-in for the installed agent\n"), 0o755); err != nil {
		h.t.Fatal(err)
	}
	a := app.New(h.agentConfig(name, token), h.log, upgrade.StartupResult{})
	a.SetUpgradeBinaryPath(bin)
	h.t.Cleanup(func() { a.Close(time.Second) })
	return a, bin
}

// assertNotSwapped checks a failed upgrade left the agent's install directory
// as it was: same binary, no pending marker, no kept report, no download.
func assertNotSwapped(t *testing.T, bin string) {
	t.Helper()
	got, err := os.ReadFile(bin)
	if err != nil || string(got) != "stand-in for the installed agent\n" {
		t.Fatalf("agent binary was changed: %q (err %v)", got, err)
	}
	entries, err := os.ReadDir(filepath.Dir(bin))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("failed upgrade left files beside the binary: %v", names)
	}
}

// TestUpgradeDownloadFailureDetail: a release that was ingested but whose
// artifact is not on disk fails at the download, and the agent's own error
// text reaches the dashboard API.
func TestUpgradeDownloadFailureDetail(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const name, version = "dl-fail", "9.9.9"

	token := h.registerDevice(name)
	if err := h.st.IngestRelease(ctx, version, runtime.GOOS, runtime.GOARCH, strings.Repeat("ab", 32), "c2ln", 1000); err != nil {
		t.Fatal(err)
	}
	// Set while the device is offline; the handshake then offers it.
	if d := h.patchTarget(name, version); d.UpgradeDispatch == nil || d.UpgradeDispatch.Code != store.DispatchNotConnected {
		t.Fatalf("offline PATCH dispatch = %+v, want %s", d.UpgradeDispatch, store.DispatchNotConnected)
	}

	a, bin := h.realUpgradeAgent(name, token)
	h.startApp(a, h.agentConfig(name, token), nil)

	waitFor(t, 10*time.Second, func() bool {
		lu := h.apiDevice(name).LatestUpgrade
		return lu != nil && lu.State == proto.UpgradeFailed
	})
	d := h.apiDevice(name)
	if d.UpgradeDispatch == nil || d.UpgradeDispatch.Code != store.DispatchSent || d.UpgradeDispatch.UpgradeID != d.LatestUpgrade.ID {
		t.Fatalf("dispatch = %+v, want sent for upgrade %s", d.UpgradeDispatch, d.LatestUpgrade.ID)
	}
	if d.LatestUpgrade.FailureReason != proto.UpgradeReasonDownloadFailed {
		t.Fatalf("summary reason = %q, want %q", d.LatestUpgrade.FailureReason, proto.UpgradeReasonDownloadFailed)
	}

	u := h.apiUpgrade(name, d.LatestUpgrade.ID)
	if u.State != proto.UpgradeFailed || u.FailureReason != proto.UpgradeReasonDownloadFailed || u.ClosedBy != store.UpgradeSourceAgent {
		t.Fatalf("upgrade = state %q reason %q closed_by %q", u.State, u.FailureReason, u.ClosedBy)
	}
	if u.ToVersion != version || u.FinishedAt == nil {
		t.Fatalf("upgrade to_version=%q finished_at=%v", u.ToVersion, u.FinishedAt)
	}
	if !strings.Contains(u.FailureDetail, "status 404") {
		t.Fatalf("detail does not name the HTTP status: %q", u.FailureDetail)
	}
	if strings.Contains(u.FailureDetail, token) {
		t.Fatalf("detail leaks the device token: %q", u.FailureDetail)
	}
	assertEventStates(t, u.Events, proto.UpgradeRequested, proto.UpgradeDownloading, proto.UpgradeFailed)
	last := u.Events[len(u.Events)-1]
	if last.Source != store.UpgradeSourceAgent || last.Reason != proto.UpgradeReasonDownloadFailed || last.Detail != u.FailureDetail || !last.Applied {
		t.Fatalf("final event = %+v", last)
	}
	if u.Events[0].Source != store.UpgradeSourceServer {
		t.Fatalf("requested event source = %q, want server", u.Events[0].Source)
	}

	// The list endpoint carries the same row.
	var list []apiUpgrade
	if code := h.api(http.MethodGet, "/api/devices/"+name+"/upgrades", nil, &list); code != http.StatusOK {
		t.Fatalf("list status %d", code)
	}
	if len(list) != 1 || list[0].ID != u.ID || list[0].FailureDetail != u.FailureDetail {
		t.Fatalf("list = %+v", list)
	}
	assertNotSwapped(t, bin)
	h.assertSlotFree()
}

// TestUpgradeResultMatchesByID: a result lands on the upgrade whose id it
// names, not on the device's newest one, and never on another device's.
func TestUpgradeResultMatchesByID(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tokenA := h.registerDevice("byid-a")
	tokenB := h.registerDevice("byid-b")
	if err := h.st.IngestRelease(ctx, "0.4.0", "linux", "amd64", "abc123", "sig1", 1000); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetDesiredAgentVersion(ctx, "byid-a", "0.4.0"); err != nil {
		t.Fatal(err)
	}

	probeA, _ := h.startProbe("byid-a", tokenA)
	oldID := probeA.waitRequest(t, 1).UpgradeID
	if oldID == "" {
		t.Fatal("upgrade_request carried no upgrade_id")
	}
	// Close the first attempt and start a second one for the same version.
	if code := h.api(http.MethodPost, "/api/devices/byid-a/upgrades/"+oldID+"/abandon", nil, nil); code != http.StatusOK {
		t.Fatalf("abandon status %d", code)
	}
	var retry struct {
		Status    string `json:"status"`
		UpgradeID string `json:"upgrade_id"`
	}
	if code := h.api(http.MethodPost, "/api/devices/byid-a/upgrades/retry", nil, &retry); code != http.StatusAccepted {
		t.Fatalf("retry status %d", code)
	}
	newID := probeA.waitRequest(t, 2).UpgradeID
	if newID == "" || newID == oldID || retry.UpgradeID != newID {
		t.Fatalf("second attempt id: request %q, retry response %q, first %q", newID, retry.UpgradeID, oldID)
	}
	if rows, _ := h.st.GetDeviceUpgrades(ctx, h.deviceID("byid-a"), 0); len(rows) != 2 {
		t.Fatalf("got %d upgrade rows, want 2", len(rows))
	}

	// The old binary reports how the first attempt really ended. Both rows
	// have the same to_version, so only the id tells them apart.
	probeA.report(t, proto.UpgradeResult{
		FromVersion: "0.3.0", ToVersion: "0.4.0", State: proto.UpgradeRolledBack,
		Reason: proto.UpgradeReasonNoHandshake, UpgradeID: oldID, Detail: "late report for the first attempt",
	})
	waitFor(t, 5*time.Second, func() bool { return h.upgradeRow(oldID).State == proto.UpgradeRolledBack })
	old := h.upgradeRow(oldID)
	if old.FailureReason != proto.UpgradeReasonNoHandshake || old.ClosedBy != store.UpgradeSourceAgent ||
		old.FailureDetail != "late report for the first attempt" {
		t.Fatalf("older upgrade = reason %q closed_by %q detail %q", old.FailureReason, old.ClosedBy, old.FailureDetail)
	}
	assertNewerUntouched := func() {
		t.Helper()
		newer := h.upgradeRow(newID)
		if newer.State != proto.UpgradeRequested || newer.FailureReason != "" || newer.FailureDetail != "" || newer.FinishedAt != nil {
			t.Fatalf("newer upgrade was touched: %+v", newer)
		}
		if ev := h.upgradeEvents(newID); len(ev) != 1 {
			t.Fatalf("newer upgrade gained timeline events: %+v", ev)
		}
	}
	assertNewerUntouched()

	// Another device naming that row's id reaches nothing.
	probeB, _ := h.startProbe("byid-b", tokenB)
	probeB.report(t, proto.UpgradeResult{
		FromVersion: "0.3.0", ToVersion: "0.4.0", State: proto.UpgradeFailed,
		Reason: proto.UpgradeReasonVerifyFailed, UpgradeID: newID, Detail: "not mine",
	})
	waitFor(t, 5*time.Second, func() bool {
		return h.auditCount("agent:byid-b", "upgrade_result_unmatched", proto.UpgradeFailed, newID) == 1
	})
	assertNewerUntouched()
	if rows, _ := h.st.GetDeviceUpgrades(ctx, h.deviceID("byid-b"), 0); len(rows) != 0 {
		t.Fatalf("unmatched result created rows for the sender: %+v", rows)
	}
	if code := h.api(http.MethodGet, "/api/devices/byid-b/upgrades/"+newID, nil, nil); code != http.StatusNotFound {
		t.Fatalf("detail of another device's upgrade: status %d, want 404", code)
	}
	if active, _ := h.st.GetActiveUpgrade(ctx); active == nil || active.ID != newID {
		t.Fatalf("active upgrade = %+v, want %s", active, newID)
	}
}

// TestUpgradeLateResultAfterAbandon: abandoning does not stop the agent. A
// progress report that arrives afterwards is kept on the timeline but cannot
// reopen the upgrade; the agent's own final report replaces the abandon.
func TestUpgradeLateResultAfterAbandon(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	token := h.registerDevice("late-a")
	if err := h.st.IngestRelease(ctx, "0.4.0", "linux", "amd64", "abc123", "sig1", 1000); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetDesiredAgentVersion(ctx, "late-a", "0.4.0"); err != nil {
		t.Fatal(err)
	}
	probe, _ := h.startProbe("late-a", token)
	id := probe.waitRequest(t, 1).UpgradeID
	result := func(state string) proto.UpgradeResult {
		return proto.UpgradeResult{FromVersion: "0.3.0", ToVersion: "0.4.0", State: state, UpgradeID: id}
	}

	probe.report(t, result(proto.UpgradeDownloading))
	waitFor(t, 5*time.Second, func() bool { return h.upgradeRow(id).State == proto.UpgradeDownloading })

	var body map[string]string
	if code := h.api(http.MethodPost, "/api/devices/late-a/upgrades/"+id+"/abandon", nil, &body); code != http.StatusOK || body["status"] != "abandoned" {
		t.Fatalf("abandon: status %d body %v", code, body)
	}
	u := h.upgradeRow(id)
	if u.State != proto.UpgradeFailed || u.FailureReason != store.UpgradeReasonAbandoned || u.ClosedBy != store.UpgradeSourceAdmin {
		t.Fatalf("after abandon: state %q reason %q closed_by %q", u.State, u.FailureReason, u.ClosedBy)
	}
	h.assertSlotFree()
	finishedAt := *u.FinishedAt

	// A second abandon is refused, and says why.
	if code := h.api(http.MethodPost, "/api/devices/late-a/upgrades/"+id+"/abandon", nil, &body); code != http.StatusConflict || body["code"] != "not_in_flight" {
		t.Fatalf("second abandon: status %d body %v", code, body)
	}

	// Late progress: recorded, not applied.
	probe.report(t, result(proto.UpgradeVerifying))
	waitFor(t, 5*time.Second, func() bool { return len(h.upgradeEvents(id)) == 4 })
	u = h.upgradeRow(id)
	if u.State != proto.UpgradeFailed || u.FailureReason != store.UpgradeReasonAbandoned || u.ClosedBy != store.UpgradeSourceAdmin ||
		u.FinishedAt == nil || !u.FinishedAt.Equal(finishedAt) {
		t.Fatalf("late verifying changed the abandoned upgrade: %+v", u)
	}
	h.assertSlotFree()

	// The agent's final word supersedes the abandon.
	probe.report(t, result(proto.UpgradeVerified))
	waitFor(t, 5*time.Second, func() bool { return h.upgradeRow(id).State == proto.UpgradeVerified })
	u = h.upgradeRow(id)
	if u.ClosedBy != store.UpgradeSourceAgent || u.FailureReason != "" || u.FinishedAt == nil {
		t.Fatalf("after verified: closed_by %q reason %q finished %v", u.ClosedBy, u.FailureReason, u.FinishedAt)
	}
	h.assertSlotFree()

	got := h.apiUpgrade("late-a", id)
	assertEventStates(t, got.Events,
		proto.UpgradeRequested, proto.UpgradeDownloading, proto.UpgradeFailed, proto.UpgradeVerifying, proto.UpgradeVerified)
	wantApplied := []bool{true, true, true, false, true}
	wantSource := []string{store.UpgradeSourceServer, store.UpgradeSourceAgent, store.UpgradeSourceAdmin, store.UpgradeSourceAgent, store.UpgradeSourceAgent}
	for i, e := range got.Events {
		if e.Applied != wantApplied[i] || e.Source != wantSource[i] {
			t.Fatalf("event %d (%s): applied=%v source=%q, want applied=%v source=%q",
				i, e.State, e.Applied, e.Source, wantApplied[i], wantSource[i])
		}
	}
	if got.State != proto.UpgradeVerified || got.ClosedBy != store.UpgradeSourceAgent {
		t.Fatalf("API upgrade = state %q closed_by %q", got.State, got.ClosedBy)
	}
}

// TestUpgradeDispatchStatus: when a target is set and nothing is dispatched,
// the device summary says why, and keeps up as that changes.
func TestUpgradeDispatchStatus(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	tokenA := h.registerDevice("disp-a")
	tokenB := h.registerDevice("disp-b")
	h.registerDevice("disp-off")
	if err := h.st.IngestRelease(ctx, "0.4.0", "linux", "amd64", "abc123", "sig1", 1000); err != nil {
		t.Fatal(err)
	}

	// An offline device.
	off := h.patchTarget("disp-off", "0.4.0")
	if off.UpgradeDispatch == nil || off.UpgradeDispatch.Code != store.DispatchNotConnected || off.UpgradeDispatch.Message == "" {
		t.Fatalf("offline dispatch = %+v, want %s", off.UpgradeDispatch, store.DispatchNotConnected)
	}
	if off.LatestUpgrade != nil {
		t.Fatalf("offline device has an upgrade: %+v", off.LatestUpgrade)
	}

	// Both agents connect before either upgrade starts: once one is in
	// flight, a handshake reads in_flight whatever the target.
	probeA, _ := h.startSettledProbe("disp-a", tokenA)
	probeB, _ := h.startSettledProbe("disp-b", tokenB)

	// First device: connected, release present, so the request goes out.
	a := h.patchTarget("disp-a", "0.4.0")
	if a.UpgradeDispatch == nil || a.UpgradeDispatch.Code != store.DispatchSent || a.UpgradeDispatch.UpgradeID == "" {
		t.Fatalf("first device dispatch = %+v, want sent with an upgrade id", a.UpgradeDispatch)
	}
	if a.LatestUpgrade == nil || a.LatestUpgrade.ID != a.UpgradeDispatch.UpgradeID || a.LatestUpgrade.State != proto.UpgradeRequested ||
		a.LatestUpgrade.FromVersion != "0.3.0" || a.LatestUpgrade.ToVersion != "0.4.0" {
		t.Fatalf("first device latest_upgrade = %+v", a.LatestUpgrade)
	}
	reqA := probeA.waitRequest(t, 1)
	if reqA.UpgradeID != a.UpgradeDispatch.UpgradeID {
		t.Fatalf("request id %q, dispatch id %q", reqA.UpgradeID, a.UpgradeDispatch.UpgradeID)
	}

	// Second device: blocked by the first, and told so by name.
	b := h.patchTarget("disp-b", "0.4.0")
	disp := b.UpgradeDispatch
	if disp == nil || disp.Code != store.DispatchInFlight || disp.BlockingDevice != "disp-a" {
		t.Fatalf("second device dispatch = %+v, want in_flight blocked by disp-a", disp)
	}
	if !strings.Contains(disp.Message, "disp-a") || disp.BlockingState != proto.UpgradeRequested || disp.BlockingSince == nil || disp.UpgradeID != "" {
		t.Fatalf("second device dispatch lacks the holder's details: %+v", disp)
	}
	if b.LatestUpgrade != nil {
		t.Fatalf("blocked device has an upgrade: %+v", b.LatestUpgrade)
	}
	if got := h.apiDevice("disp-b").UpgradeDispatch; got == nil || got.Code != store.DispatchInFlight || got.BlockingDevice != "disp-a" {
		t.Fatalf("GET /api/devices dispatch = %+v", got)
	}

	// An explicit retry is refused the same way.
	var refused struct {
		Error          string `json:"error"`
		Code           string `json:"code"`
		BlockingDevice string `json:"blocking_device"`
	}
	if code := h.api(http.MethodPost, "/api/devices/disp-b/upgrades/retry", nil, &refused); code != http.StatusConflict {
		t.Fatalf("retry while blocked: status %d", code)
	}
	if refused.Code != store.DispatchInFlight || refused.BlockingDevice != "disp-a" || !strings.HasPrefix(refused.Error, "retry not dispatched: ") {
		t.Fatalf("retry refusal = %+v", refused)
	}
	if n := len(probeB.requests()); n != 0 {
		t.Fatalf("blocked device was sent %d upgrade request(s)", n)
	}

	// The first upgrade ends: the second device is waiting, not re-offered.
	probeA.report(t, proto.UpgradeResult{
		FromVersion: "0.3.0", ToVersion: "0.4.0", State: proto.UpgradeFailed,
		Reason: proto.UpgradeReasonSelftestFailed, UpgradeID: reqA.UpgradeID,
	})
	waitFor(t, 5*time.Second, func() bool { return h.dispatchCode("disp-b") == store.DispatchWaiting })
	if w := h.apiDevice("disp-b").UpgradeDispatch; w.BlockingDevice != "" || w.BlockingSince != nil || w.Message == "" {
		t.Fatalf("waiting dispatch = %+v", w)
	}
	if n := len(probeB.requests()); n != 0 {
		t.Fatalf("waiting device was sent %d upgrade request(s)", n)
	}
	h.assertSlotFree()
	// The device whose upgrade failed keeps reading sent, and the offline
	// one is unaffected.
	if got := h.dispatchCode("disp-a"); got != store.DispatchSent {
		t.Fatalf("first device dispatch after its failure = %q, want sent", got)
	}
	if got := h.dispatchCode("disp-off"); got != store.DispatchNotConnected {
		t.Fatalf("offline device dispatch = %q, want not_connected", got)
	}

	// Upgrade now on the waiting device dispatches.
	if code := h.api(http.MethodPost, "/api/devices/disp-b/upgrades/retry", nil, nil); code != http.StatusAccepted {
		t.Fatalf("retry after the slot freed: status %d", code)
	}
	reqB := probeB.waitRequest(t, 1)
	if got := h.apiDevice("disp-b").UpgradeDispatch; got == nil || got.Code != store.DispatchSent || got.UpgradeID != reqB.UpgradeID {
		t.Fatalf("dispatch after retry = %+v, want sent for %s", got, reqB.UpgradeID)
	}

	// Clearing the target clears the status.
	if cleared := h.patchTarget("disp-off", ""); cleared.UpgradeDispatch != nil {
		t.Fatalf("dispatch after clearing the target = %+v, want null", cleared.UpgradeDispatch)
	}
}

// staleFixture has one device whose upgrade sits at requested and a second
// device blocked behind it.
type staleFixture struct {
	h      *harness
	probe  *upgradeProbe
	id     string // the in-flight upgrade
	holder string
	waiter string
}

func newStaleFixture(t *testing.T, prefix string) *staleFixture {
	t.Helper()
	h := newHarness(t)
	ctx := context.Background()
	f := &staleFixture{h: h, holder: prefix + "-a", waiter: prefix + "-b"}
	tokenA := h.registerDevice(f.holder)
	tokenB := h.registerDevice(f.waiter)
	if err := h.st.IngestRelease(ctx, "0.4.0", "linux", "amd64", "abc123", "sig1", 1000); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{f.holder, f.waiter} {
		if err := h.st.SetDesiredAgentVersion(ctx, name, "0.4.0"); err != nil {
			t.Fatal(err)
		}
	}
	f.probe, _ = h.startProbe(f.holder, tokenA)
	f.id = f.probe.waitRequest(t, 1).UpgradeID
	// The second device's handshake finds the slot taken.
	h.startProbe(f.waiter, tokenB)
	waitFor(t, 5*time.Second, func() bool { return h.dispatchCode(f.waiter) == store.DispatchInFlight })
	return f
}

// TestUpgradeStaleRequestedTimesOut: an upgrade the agent never acted on is
// closed by the sweep, which frees the fleet slot.
func TestUpgradeStaleRequestedTimesOut(t *testing.T) {
	f := newStaleFixture(t, "stale")
	h := f.h
	ctx := context.Background()
	base := h.upgradeRow(f.id).UpdatedAt

	// Inside the limit nothing happens.
	h.hub.SweepUpgrades(ctx, base.Add(time.Minute))
	if u := h.upgradeRow(f.id); u.State != proto.UpgradeRequested {
		t.Fatalf("swept early: state %q", u.State)
	}
	if got := h.dispatchCode(f.waiter); got != store.DispatchInFlight {
		t.Fatalf("waiter dispatch = %q, want in_flight", got)
	}

	clock := base.Add(3 * time.Minute)
	h.hub.SweepUpgrades(ctx, clock)
	u := h.upgradeRow(f.id)
	if u.State != proto.UpgradeFailed || u.FailureReason != store.UpgradeReasonTimedOut || u.ClosedBy != store.UpgradeSourceServer {
		t.Fatalf("after sweep: state %q reason %q closed_by %q", u.State, u.FailureReason, u.ClosedBy)
	}
	if want := "no report from the agent for 3m while requested"; u.FailureDetail != want {
		t.Fatalf("detail = %q, want %q", u.FailureDetail, want)
	}
	if u.FinishedAt == nil || !u.FinishedAt.Equal(clock) {
		t.Fatalf("finished_at = %v, want the sweep's clock %v", u.FinishedAt, clock)
	}
	h.assertSlotFree()
	if got := h.dispatchCode(f.waiter); got != store.DispatchWaiting {
		t.Fatalf("waiter dispatch after timeout = %q, want waiting", got)
	}
	if lu := h.apiDevice(f.holder).LatestUpgrade; lu == nil || lu.State != proto.UpgradeFailed || lu.FailureReason != store.UpgradeReasonTimedOut {
		t.Fatalf("holder latest_upgrade = %+v", lu)
	}
	// Sweeping again finds nothing to do.
	h.hub.SweepUpgrades(ctx, clock.Add(time.Hour))
	if ev := h.upgradeEvents(f.id); len(ev) != 2 {
		t.Fatalf("timeline = %+v, want requested then failed", ev)
	}

	// The agent was only slow. Its late progress cannot reopen the upgrade;
	// its final report supersedes the timeout.
	f.probe.report(t, proto.UpgradeResult{FromVersion: "0.3.0", ToVersion: "0.4.0", State: proto.UpgradeDownloading, UpgradeID: f.id})
	f.probe.report(t, proto.UpgradeResult{
		FromVersion: "0.3.0", ToVersion: "0.4.0", State: proto.UpgradeFailed,
		Reason: proto.UpgradeReasonDownloadFailed, UpgradeID: f.id, Detail: "GET /x: status 503",
	})
	waitFor(t, 5*time.Second, func() bool { return h.upgradeRow(f.id).ClosedBy == store.UpgradeSourceAgent })
	u = h.upgradeRow(f.id)
	if u.State != proto.UpgradeFailed || u.FailureReason != proto.UpgradeReasonDownloadFailed || u.FailureDetail != "GET /x: status 503" {
		t.Fatalf("after the agent's report: %+v", u)
	}
	ev := h.upgradeEvents(f.id)
	if len(ev) != 4 || ev[2].State != proto.UpgradeDownloading || ev[2].Applied || !ev[3].Applied {
		t.Fatalf("timeline after late reports = %+v", ev)
	}
	h.assertSlotFree()
}

// TestUpgradeStalledRestarting: after the swap an upgrade that stops
// reporting is never timed out. It keeps the fleet slot and is flagged as
// stalled until an operator abandons it.
func TestUpgradeStalledRestarting(t *testing.T) {
	f := newStaleFixture(t, "stall")
	h := f.h
	ctx := context.Background()
	result := func(state string) proto.UpgradeResult {
		return proto.UpgradeResult{FromVersion: "0.3.0", ToVersion: "0.4.0", State: state, UpgradeID: f.id}
	}
	for _, s := range []string{proto.UpgradeDownloading, proto.UpgradeVerifying, proto.UpgradeSelftest, proto.UpgradeSwapped, proto.UpgradeRestarting} {
		f.probe.report(t, result(s))
	}
	waitFor(t, 5*time.Second, func() bool { return h.upgradeRow(f.id).State == proto.UpgradeRestarting })
	base := h.upgradeRow(f.id).UpdatedAt
	nEvents := len(h.upgradeEvents(f.id))
	if nEvents != 6 {
		t.Fatalf("got %d timeline events, want 6", nEvents)
	}

	assertHeld := func(when string) {
		t.Helper()
		u := h.upgradeRow(f.id)
		if u.State != proto.UpgradeRestarting || u.FinishedAt != nil || u.ClosedBy != "" || !u.UpdatedAt.Equal(base) {
			t.Fatalf("%s: upgrade changed: %+v", when, u)
		}
		if n := len(h.upgradeEvents(f.id)); n != nEvents {
			t.Fatalf("%s: timeline has %d events, want %d", when, n, nEvents)
		}
		if active, _ := h.st.GetActiveUpgrade(ctx); active == nil || active.ID != f.id {
			t.Fatalf("%s: slot holder = %+v, want %s", when, active, f.id)
		}
		if got := h.dispatchCode(f.waiter); got != store.DispatchInFlight {
			t.Fatalf("%s: waiter dispatch = %q, want in_flight", when, got)
		}
	}

	// Within the 5 minute limit: not stalled, and nothing is published.
	published := h.events.count("upgrade:" + f.holder)
	early := base.Add(4 * time.Minute)
	h.hub.SweepUpgrades(ctx, early)
	assertHeld("sweep at 4m")
	if h.upgradeRow(f.id).IsStalled(early) {
		t.Fatal("stalled before the limit")
	}
	if got := h.events.count("upgrade:" + f.holder); got != published {
		t.Fatalf("sweep inside the limit published %d UpgradeChanged event(s)", got-published)
	}

	// Past it: still in flight, flagged, and announced on every sweep.
	late := base.Add(6 * time.Minute)
	for i := 1; i <= 2; i++ {
		h.hub.SweepUpgrades(ctx, late)
		assertHeld("sweep at 6m")
		if got := h.events.count("upgrade:" + f.holder); got != published+i {
			t.Fatalf("after %d late sweep(s): %d UpgradeChanged event(s), want %d", i, got-published, i)
		}
	}
	if !h.upgradeRow(f.id).IsStalled(late) {
		t.Fatal("not stalled 6m after the last report")
	}
	// Far past it, the answer is the same: no timeout after the swap.
	h.hub.SweepUpgrades(ctx, base.Add(48*time.Hour))
	assertHeld("sweep at 48h")

	// The API computes the flag from the wall clock, which has not moved.
	if u := h.apiUpgrade(f.holder, f.id); u.Stalled || u.State != proto.UpgradeRestarting {
		t.Fatalf("API upgrade at real time: stalled=%v state=%q", u.Stalled, u.State)
	}

	// The operator's way out is Abandon, which frees the slot.
	if code := h.api(http.MethodPost, "/api/devices/"+f.holder+"/upgrades/"+f.id+"/abandon", nil, nil); code != http.StatusOK {
		t.Fatalf("abandon status %d", code)
	}
	h.assertSlotFree()
	waitFor(t, 5*time.Second, func() bool { return h.dispatchCode(f.waiter) == store.DispatchWaiting })
}

// TestUpgradeStalledFlagInAPI: an upgrade whose last report really is old
// reads stalled on the detail endpoint, the list and the device summary.
func TestUpgradeStalledFlagInAPI(t *testing.T) {
	f := newStaleFixture(t, "flag")
	h := f.h
	ctx := context.Background()

	// The agent's restarting report, dated ten minutes ago.
	then := time.Now().Add(-10 * time.Minute)
	if applied, err := h.st.RecordUpgradeEvent(ctx, store.UpgradeEvent{
		UpgradeID: f.id, TS: then, Source: store.UpgradeSourceAgent, State: proto.UpgradeRestarting,
	}); err != nil || !applied {
		t.Fatalf("record restarting: applied=%v err=%v", applied, err)
	}
	h.hub.SweepUpgrades(ctx, time.Now())

	u := h.apiUpgrade(f.holder, f.id)
	if !u.Stalled || u.State != proto.UpgradeRestarting || u.FinishedAt != nil {
		t.Fatalf("detail: stalled=%v state=%q finished=%v", u.Stalled, u.State, u.FinishedAt)
	}
	var list []apiUpgrade
	if code := h.api(http.MethodGet, "/api/devices/"+f.holder+"/upgrades?limit=5", nil, &list); code != http.StatusOK || len(list) != 1 || !list[0].Stalled {
		t.Fatalf("list: status %d, %+v", code, list)
	}
	if lu := h.apiDevice(f.holder).LatestUpgrade; lu == nil || !lu.Stalled || lu.State != proto.UpgradeRestarting {
		t.Fatalf("summary latest_upgrade = %+v", lu)
	}
	// The blocked device still names the holder.
	if d := h.apiDevice(f.waiter).UpgradeDispatch; d == nil || d.Code != store.DispatchInFlight || d.BlockingDevice != f.holder {
		t.Fatalf("waiter dispatch = %+v", d)
	}
}

// legacyUpgradeResult is upgrade_result as agents sent it before upgrade_id
// and detail existed.
type legacyUpgradeResult struct {
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
	State       string `json:"state"`
	Reason      string `json:"reason,omitempty"`
}

// TestOldAgentUpgradeResultOnNewServer: results without upgrade_id are
// matched by to_version, in-flight attempt first, and still drive an upgrade
// to verified.
func TestOldAgentUpgradeResultOnNewServer(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	token := h.registerDevice("old-a")
	if err := h.st.IngestRelease(ctx, "0.4.0", "linux", "amd64", "abc123", "sig1", 1000); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetDesiredAgentVersion(ctx, "old-a", "0.4.0"); err != nil {
		t.Fatal(err)
	}
	probe, _ := h.startProbe("old-a", token)
	legacy := func(state, reason string) legacyUpgradeResult {
		return legacyUpgradeResult{FromVersion: "0.3.0", ToVersion: "0.4.0", State: state, Reason: reason}
	}

	// A first attempt fails, so a closed row for the same version exists.
	firstID := probe.waitRequest(t, 1).UpgradeID
	probe.report(t, legacy(proto.UpgradeDownloading, ""))
	probe.report(t, legacy(proto.UpgradeFailed, proto.UpgradeReasonSelftestFailed))
	waitFor(t, 5*time.Second, func() bool { return h.upgradeRow(firstID).State == proto.UpgradeFailed })

	if code := h.api(http.MethodPost, "/api/devices/old-a/upgrades/retry", nil, nil); code != http.StatusAccepted {
		t.Fatalf("retry status %d", code)
	}
	secondID := probe.waitRequest(t, 2).UpgradeID
	if secondID == "" || secondID == firstID {
		t.Fatalf("second attempt id %q (first %q)", secondID, firstID)
	}
	steps := []string{
		proto.UpgradeDownloading, proto.UpgradeVerifying, proto.UpgradeSelftest,
		proto.UpgradeSwapped, proto.UpgradeRestarting, proto.UpgradeVerified,
	}
	for _, s := range steps {
		probe.report(t, legacy(s, ""))
	}
	waitFor(t, 5*time.Second, func() bool { return h.upgradeRow(secondID).State == proto.UpgradeVerified })

	u := h.apiUpgrade("old-a", secondID)
	if u.State != proto.UpgradeVerified || u.ClosedBy != store.UpgradeSourceAgent || u.FinishedAt == nil ||
		u.FailureReason != "" || u.FailureDetail != "" {
		t.Fatalf("second attempt = %+v", u)
	}
	assertEventStates(t, u.Events, append([]string{proto.UpgradeRequested}, steps...)...)
	for _, e := range u.Events {
		if !e.Applied || e.Detail != "" {
			t.Fatalf("event %+v, want applied with no detail", e)
		}
	}

	// The failed first attempt took none of the second attempt's results.
	first := h.apiUpgrade("old-a", firstID)
	if first.State != proto.UpgradeFailed || first.FailureReason != proto.UpgradeReasonSelftestFailed || first.FailureDetail != "" {
		t.Fatalf("first attempt = %+v", first)
	}
	assertEventStates(t, first.Events, proto.UpgradeRequested, proto.UpgradeDownloading, proto.UpgradeFailed)
	h.assertSlotFree()

	// A result for a version the device was never offered matches nothing.
	probe.report(t, legacyUpgradeResult{FromVersion: "0.3.0", ToVersion: "7.7.7", State: proto.UpgradeFailed, Reason: proto.UpgradeReasonVerifyFailed})
	waitFor(t, 5*time.Second, func() bool {
		return h.auditCount("agent:old-a", "upgrade_result_unmatched", proto.UpgradeFailed, "") == 1
	})
	if rows, _ := h.st.GetDeviceUpgrades(ctx, h.deviceID("old-a"), 0); len(rows) != 2 {
		t.Fatalf("got %d upgrade rows, want 2", len(rows))
	}
}
