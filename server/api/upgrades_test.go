package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

// The documented field sets. A change here is a change to the dashboard's
// contract.
var (
	upgradeKeys = []string{"closed_by", "device_id", "failure_detail", "failure_reason", "finished_at", "from_version",
		"id", "requested_by", "stalled", "started_at", "state", "to_version", "updated_at"}
	eventKeys = []string{"applied", "detail", "reason", "source", "state", "ts"}
)

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// decode reads a response body as JSON into v after checking the status.
func decode(t *testing.T, resp *http.Response, status int, v any) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d", resp.StatusCode, status)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

// connectAgent registers a device and connects it to the hub over a real
// agent socket, as linux/amd64 at version 0.1.0. Agent traffic authenticates
// with the device token, never the dashboard session.
func (e *env) connectAgent(t *testing.T, name string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, token, err := e.st.CreateDevice(ctx, name, "", false, store.Schedule{})
	if err != nil {
		t.Fatal(err)
	}
	ws := httptest.NewServer(e.hub)
	t.Cleanup(ws.Close)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ws.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	hello, _ := proto.New(proto.TypeHello, proto.Hello{ProtocolVersion: proto.Version, AgentVersion: "0.1.0",
		DeviceName: name, Token: token, OS: "linux", Arch: "amd64"})
	b, _ := proto.Encode(hello)
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	if _, data, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	} else if ack, err := proto.Decode(data); err != nil || ack.Type != proto.TypeHelloAck {
		t.Fatalf("handshake: %+v err=%v", ack, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !e.hub.Connected(name) {
		if time.Now().After(deadline) {
			t.Fatalf("%s never connected", name)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return conn
}

// closedUpgrade creates an upgrade for the device and fails it.
func closedUpgrade(t *testing.T, st *store.Store, deviceID int64, detail string) string {
	t.Helper()
	ctx := context.Background()
	id, err := st.CreateUpgrade(ctx, deviceID, "0.2.4", "0.2.5", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordUpgradeEvent(ctx, store.UpgradeEvent{UpgradeID: id, Source: store.UpgradeSourceAgent,
		State: proto.UpgradeFailed, Reason: proto.UpgradeReasonSelftestConfigRejected, Detail: detail}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUpgradeRoutesNeedSession(t *testing.T) {
	e := newEnv(t)
	e.st.CreateDevice(context.Background(), "dev", "", false, store.Schedule{})
	for _, c := range [][2]string{
		{"GET", "/api/devices/dev/upgrades"},
		{"GET", "/api/devices/dev/upgrades/01ARZ3NDEKTSV4RRFFQ69G5FAV"},
		{"POST", "/api/devices/dev/upgrades/retry"},
		{"POST", "/api/devices/dev/upgrades/01ARZ3NDEKTSV4RRFFQ69G5FAV/abandon"},
	} {
		resp := e.do(t, c[0], c[1], nil)
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("%s %s: %d", c[0], c[1], resp.StatusCode)
		}
	}
}

func TestUpgradeListLimit(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	ctx := context.Background()
	d, _, _ := e.st.CreateDevice(ctx, "dev", "", false, store.Schedule{})
	var newest string
	for range 105 {
		newest = closedUpgrade(t, e.st, d.ID, "stderr text")
	}
	for _, c := range []struct {
		query string
		want  int
	}{
		{"", 20}, {"?limit=5", 5}, {"?limit=100", 100}, {"?limit=500", 100},
		{"?limit=0", 20}, {"?limit=-3", 20}, {"?limit=abc", 20},
	} {
		var list []map[string]any
		decode(t, e.do(t, "GET", "/api/devices/dev/upgrades"+c.query, nil), 200, &list)
		if len(list) != c.want {
			t.Errorf("%q: %d rows, want %d", c.query, len(list), c.want)
			continue
		}
		if list[0]["id"] != newest {
			t.Errorf("%q: first row is not the newest", c.query)
		}
	}

	var list []map[string]any
	decode(t, e.do(t, "GET", "/api/devices/dev/upgrades?limit=1", nil), 200, &list)
	u := list[0]
	if got := sortedKeys(u); !slices.Equal(got, upgradeKeys) {
		t.Fatalf("keys = %v, want %v", got, upgradeKeys)
	}
	if u["state"] != "failed" || u["failure_reason"] != "selftest_config_rejected" || u["failure_detail"] != "stderr text" ||
		u["closed_by"] != "agent" || u["stalled"] != false || u["device_id"] != float64(d.ID) || u["requested_by"] != "admin" {
		t.Fatalf("row = %v", u)
	}
	for _, k := range []string{"started_at", "updated_at", "finished_at"} {
		s, _ := u[k].(string)
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			t.Errorf("%s = %v: not RFC 3339", k, u[k])
		}
	}

	// A device with no upgrades lists [], not null; an unknown one is 404.
	e.st.CreateDevice(ctx, "fresh", "", false, store.Schedule{})
	resp := e.do(t, "GET", "/api/devices/fresh/upgrades", nil)
	var raw json.RawMessage
	decode(t, resp, 200, &raw)
	if strings.TrimSpace(string(raw)) != "[]" {
		t.Fatalf("empty list = %s", raw)
	}
	resp = e.do(t, "GET", "/api/devices/nope/upgrades", nil)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown device: %d", resp.StatusCode)
	}
}

func TestUpgradeDetail(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	ctx := context.Background()
	d, _, _ := e.st.CreateDevice(ctx, "dev", "", false, store.Schedule{})
	other, _, _ := e.st.CreateDevice(ctx, "other", "", false, store.Schedule{})
	id, _ := e.st.CreateUpgrade(ctx, d.ID, "0.2.4", "0.2.5", "admin")
	e.st.RecordUpgradeEvent(ctx, store.UpgradeEvent{UpgradeID: id, Source: store.UpgradeSourceAgent, State: proto.UpgradeDownloading})
	e.st.RecordUpgradeEvent(ctx, store.UpgradeEvent{UpgradeID: id, Source: store.UpgradeSourceAgent, State: proto.UpgradeFailed,
		Reason: proto.UpgradeReasonSelftestConfigRejected, Detail: "selftest of 0.2.5 exited 1\n<img onerror=x>"})
	// A late report, recorded but not applied.
	e.st.RecordUpgradeEvent(ctx, store.UpgradeEvent{UpgradeID: id, Source: store.UpgradeSourceAgent, State: proto.UpgradeVerifying})
	otherID := closedUpgrade(t, e.st, other.ID, "")

	var u map[string]any
	decode(t, e.do(t, "GET", "/api/devices/dev/upgrades/"+id, nil), 200, &u)
	if got, want := sortedKeys(u), slices.Sorted(slices.Values(append([]string{"events"}, upgradeKeys...))); !slices.Equal(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	if u["id"] != id || u["state"] != "failed" || u["closed_by"] != "agent" ||
		u["failure_detail"] != "selftest of 0.2.5 exited 1\n<img onerror=x>" {
		t.Fatalf("upgrade = %v", u)
	}
	events, _ := u["events"].([]any)
	if len(events) != 4 {
		t.Fatalf("events = %v", u["events"])
	}
	type want struct {
		source, state, reason string
		applied               bool
	}
	prev := ""
	for i, w := range []want{
		{"server", "requested", "", true},
		{"agent", "downloading", "", true},
		{"agent", "failed", "selftest_config_rejected", true},
		{"agent", "verifying", "", false},
	} {
		ev, _ := events[i].(map[string]any)
		if got := sortedKeys(ev); !slices.Equal(got, eventKeys) {
			t.Fatalf("event keys = %v, want %v", got, eventKeys)
		}
		if ev["source"] != w.source || ev["state"] != w.state || ev["reason"] != w.reason || ev["applied"] != w.applied {
			t.Errorf("event %d = %v, want %+v", i, ev, w)
		}
		ts, _ := ev["ts"].(string)
		at, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			t.Errorf("event %d ts = %v: not RFC 3339", i, ev["ts"])
		}
		if s := at.Format(time.RFC3339Nano); s < prev {
			t.Errorf("event %d ts goes backwards", i)
		} else {
			prev = s
		}
	}
	if ev := events[2].(map[string]any); ev["detail"] != u["failure_detail"] {
		t.Errorf("failed event detail = %v", ev["detail"])
	}

	// 404 unless the id belongs to the named device.
	for _, path := range []string{
		"/api/devices/dev/upgrades/" + otherID,
		"/api/devices/other/upgrades/" + id,
		"/api/devices/dev/upgrades/01ARZ3NDEKTSV4RRFFQ69G5FAV",
		"/api/devices/nope/upgrades/" + id,
	} {
		var body map[string]any
		decode(t, e.do(t, "GET", path, nil), 404, &body)
		if len(body) != 1 || body["error"] != "not found" {
			t.Errorf("%s: body = %v", path, body)
		}
	}
}

// summaryUpgradeFields is the part of the device summary these tests read.
type summaryUpgradeFields struct {
	UpgradeDispatch map[string]any `json:"upgrade_dispatch"`
	LatestUpgrade   map[string]any `json:"latest_upgrade"`
}

func (e *env) patchVersion(t *testing.T, name, version string) summaryUpgradeFields {
	t.Helper()
	var sum summaryUpgradeFields
	decode(t, e.do(t, "PATCH", "/api/devices/"+name, map[string]any{"desired_agent_version": version}), 200, &sum)
	return sum
}

func (e *env) summary(t *testing.T, name string) summaryUpgradeFields {
	t.Helper()
	var list []struct {
		Name string `json:"name"`
		summaryUpgradeFields
	}
	decode(t, e.do(t, "GET", "/api/devices", nil), 200, &list)
	for _, s := range list {
		if s.Name == name {
			return s.summaryUpgradeFields
		}
	}
	t.Fatalf("device %s not listed", name)
	return summaryUpgradeFields{}
}

var (
	dispatchKeys        = []string{"at", "blocking_device", "code", "message", "upgrade_id"}
	dispatchBlockedKeys = []string{"at", "blocking_device", "blocking_since", "blocking_state", "code", "message", "upgrade_id"}
)

// PATCH answers with what came of the offer, for each way it can go, and
// retry and abandon report refusals in full.
func TestPatchUpgradeDispatchRetryAbandon(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	ctx := context.Background()
	e.st.IngestRelease(ctx, "0.2.0", "linux", "amd64", "abc", "sig", 100)
	e.connectAgent(t, "dev-a")
	e.connectAgent(t, "dev-b")
	e.connectAgent(t, "dev-c")
	e.st.CreateDevice(ctx, "dev-off", "", false, store.Schedule{})

	// Before any target is set there is nothing to report.
	if sum := e.summary(t, "dev-a"); sum.UpgradeDispatch != nil || sum.LatestUpgrade != nil {
		t.Fatalf("fresh device = %+v", sum)
	}

	// No release for the target.
	sum := e.patchVersion(t, "dev-c", "9.9.9")
	if d := sum.UpgradeDispatch; d == nil || d["code"] != "no_release" || d["message"] == "" || d["blocking_device"] != "" || d["upgrade_id"] != "" {
		t.Fatalf("no release: %v", d)
	}
	if got := sortedKeys(sum.UpgradeDispatch); !slices.Equal(got, dispatchKeys) {
		t.Fatalf("dispatch keys = %v", got)
	}
	if sum.LatestUpgrade != nil {
		t.Fatalf("no release created an upgrade: %v", sum.LatestUpgrade)
	}

	// Offline.
	sum = e.patchVersion(t, "dev-off", "0.2.0")
	if d := sum.UpgradeDispatch; d == nil || d["code"] != "not_connected" || d["message"] != "device is not connected" {
		t.Fatalf("offline: %v", d)
	}

	// Connected, release present: sent, and the summary carries the upgrade.
	sum = e.patchVersion(t, "dev-a", "0.2.0")
	d := sum.UpgradeDispatch
	if d == nil || d["code"] != "sent" || d["blocking_device"] != "" {
		t.Fatalf("connected: %v", d)
	}
	idA, _ := d["upgrade_id"].(string)
	if at, _ := d["at"].(string); idA == "" || at == "" {
		t.Fatalf("connected: %v", d)
	} else if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Fatalf("at = %q: not RFC 3339", at)
	}
	lu := sum.LatestUpgrade
	if lu == nil || lu["id"] != idA || lu["state"] != "requested" || lu["from_version"] != "0.1.0" || lu["to_version"] != "0.2.0" ||
		lu["failure_reason"] != "" || lu["stalled"] != false {
		t.Fatalf("latest_upgrade = %v", lu)
	}
	if got := sortedKeys(lu); !slices.Equal(got, []string{"failure_reason", "from_version", "id", "stalled", "state", "to_version", "updated_at"}) {
		t.Fatalf("latest_upgrade keys = %v", got)
	}

	// Blocked by dev-a.
	sum = e.patchVersion(t, "dev-b", "0.2.0")
	d = sum.UpgradeDispatch
	if d == nil || d["code"] != "in_flight" || d["blocking_device"] != "dev-a" || d["blocking_state"] != "requested" || d["upgrade_id"] != "" {
		t.Fatalf("blocked: %v", d)
	}
	msg, _ := d["message"].(string)
	if !strings.HasPrefix(msg, "another upgrade is in flight: dev-a 0.1.0 -> 0.2.0, requested for ") {
		t.Fatalf("blocked message = %q", msg)
	}
	if got := sortedKeys(d); !slices.Equal(got, dispatchBlockedKeys) {
		t.Fatalf("blocked dispatch keys = %v", got)
	}
	if since, _ := d["blocking_since"].(string); since == "" {
		t.Fatalf("blocking_since = %v", d["blocking_since"])
	} else if _, err := time.Parse(time.RFC3339, since); err != nil {
		t.Fatalf("blocking_since = %q: not RFC 3339", since)
	}
	if sum.LatestUpgrade != nil {
		t.Fatalf("blocked device got an upgrade: %v", sum.LatestUpgrade)
	}
	// GET /api/devices carries the same fields.
	if got := e.summary(t, "dev-b"); got.UpgradeDispatch["code"] != "in_flight" {
		t.Fatalf("list summary = %+v", got)
	}

	// Retry while blocked: 409 naming the holder.
	var refused map[string]any
	decode(t, e.do(t, "POST", "/api/devices/dev-b/upgrades/retry", nil), 409, &refused)
	if got := sortedKeys(refused); !slices.Equal(got, []string{"blocking_device", "code", "error"}) {
		t.Fatalf("retry 409 keys = %v", got)
	}
	errText, _ := refused["error"].(string)
	if refused["code"] != "in_flight" || refused["blocking_device"] != "dev-a" ||
		!strings.HasPrefix(errText, "retry not dispatched: another upgrade is in flight: dev-a 0.1.0 -> 0.2.0, requested for ") {
		t.Fatalf("retry 409 = %v", refused)
	}
	// Other refusals carry their code and no blocking device.
	refused = nil
	decode(t, e.do(t, "POST", "/api/devices/dev-off/upgrades/retry", nil), 409, &refused)
	if got := sortedKeys(refused); !slices.Equal(got, []string{"code", "error"}) ||
		refused["code"] != "not_connected" || refused["error"] != "retry not dispatched: device is not connected" {
		t.Fatalf("offline retry 409 = %v", refused)
	}

	// Abandon: wrong device and unknown id are 404 and change nothing.
	for _, path := range []string{
		"/api/devices/dev-b/upgrades/" + idA + "/abandon",
		"/api/devices/dev-a/upgrades/01ARZ3NDEKTSV4RRFFQ69G5FAV/abandon",
		"/api/devices/nope/upgrades/" + idA + "/abandon",
	} {
		var body map[string]any
		decode(t, e.do(t, "POST", path, nil), 404, &body)
		if len(body) != 1 || body["error"] != "not found" {
			t.Fatalf("%s: body = %v", path, body)
		}
	}
	if u, _ := e.st.GetUpgrade(ctx, idA); !u.IsInFlight() {
		t.Fatalf("refused abandon closed the upgrade: %+v", u)
	}

	var ok map[string]any
	decode(t, e.do(t, "POST", "/api/devices/dev-a/upgrades/"+idA+"/abandon", nil), 200, &ok)
	if len(ok) != 1 || ok["status"] != "abandoned" {
		t.Fatalf("abandon 200 = %v", ok)
	}
	if u, _ := e.st.GetUpgrade(ctx, idA); u.State != proto.UpgradeFailed || u.FailureReason != store.UpgradeReasonAbandoned || u.ClosedBy != store.UpgradeSourceAdmin {
		t.Fatalf("abandoned row = %+v", u)
	}
	if lu := e.summary(t, "dev-a").LatestUpgrade; lu["state"] != "failed" || lu["failure_reason"] != "abandoned" {
		t.Fatalf("holder summary = %v", lu)
	}
	// The device it was blocking is told the slot is free; nothing is sent.
	if d := e.summary(t, "dev-b"); d.UpgradeDispatch["code"] != "waiting" || d.UpgradeDispatch["blocking_device"] != "" || d.LatestUpgrade != nil {
		t.Fatalf("after abandon, blocked device = %+v", d)
	}
	if got := sortedKeys(e.summary(t, "dev-b").UpgradeDispatch); !slices.Equal(got, dispatchKeys) {
		t.Fatalf("waiting dispatch keys = %v", got)
	}

	// Abandoning it again: 409.
	var notInFlight map[string]any
	decode(t, e.do(t, "POST", "/api/devices/dev-a/upgrades/"+idA+"/abandon", nil), 409, &notInFlight)
	if len(notInFlight) != 2 || notInFlight["error"] != "upgrade is not in flight" || notInFlight["code"] != "not_in_flight" {
		t.Fatalf("abandon 409 = %v", notInFlight)
	}

	// Retry now goes out: 202 with the new upgrade's id.
	var accepted map[string]any
	decode(t, e.do(t, "POST", "/api/devices/dev-b/upgrades/retry", nil), 202, &accepted)
	idB, _ := accepted["upgrade_id"].(string)
	if len(accepted) != 2 || accepted["status"] != "retry requested" || idB == "" || idB == idA {
		t.Fatalf("retry 202 = %v", accepted)
	}
	if got := e.summary(t, "dev-b"); got.UpgradeDispatch["code"] != "sent" || got.UpgradeDispatch["upgrade_id"] != idB || got.LatestUpgrade["id"] != idB {
		t.Fatalf("after retry = %+v", got)
	}

	// Clearing the target clears the status.
	if sum := e.patchVersion(t, "dev-c", ""); sum.UpgradeDispatch != nil {
		t.Fatalf("cleared target: %v", sum.UpgradeDispatch)
	}
}

// An automatic offer is declined after a failure at the same version; the
// PATCH response says so.
func TestPatchUpgradeDispatchPrevFailed(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	ctx := context.Background()
	e.st.IngestRelease(ctx, "0.2.0", "linux", "amd64", "abc", "sig", 100)
	e.connectAgent(t, "dev")
	d, _ := e.st.GetDevice(ctx, "dev")
	id, _ := e.st.CreateUpgrade(ctx, d.ID, "0.1.0", "0.2.0", "admin")
	e.st.RecordUpgradeEvent(ctx, store.UpgradeEvent{UpgradeID: id, Source: store.UpgradeSourceAgent,
		State: proto.UpgradeFailed, Reason: proto.UpgradeReasonVerifyFailed})
	sum := e.patchVersion(t, "dev", "0.2.0")
	if d := sum.UpgradeDispatch; d == nil || d["code"] != "prev_failed" {
		t.Fatalf("dispatch = %v", d)
	}
	if sum.LatestUpgrade["id"] != id {
		t.Fatalf("a new upgrade was created: %v", sum.LatestUpgrade)
	}
}
