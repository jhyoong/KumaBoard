package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jhyoong/KumaBoard/server/store"
)

// Regression: a partial PATCH must not wipe fields it does not carry.
// Upstream bug: PATCH {"desired_agent_version": v} reset mac to "" and
// normally_off to false, destroying WOL for the device.
func TestPatchPreservesUnsetFields(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	ctx := context.Background()
	sched := store.Schedule{
		ExpectedOffline: []store.Window{{Days: "sun", From: "01:00", To: "05:00"}},
		GracePeriodS:    120,
	}
	if _, _, err := e.st.CreateDevice(ctx, "win-zeal", "aa:bb:cc:dd:ee:ff", true, sched); err != nil {
		t.Fatal(err)
	}
	if err := e.st.UpdateDeviceSettings(ctx, "win-zeal", "aa:bb:cc:dd:ee:ff", true, sched, true); err != nil {
		t.Fatal(err)
	}

	// (a) Upgrade-panel PATCH: only desired_agent_version. The device has no
	// live hub session, so tryUpgrade is a no-op here.
	resp := e.do(t, "PATCH", "/api/devices/win-zeal", map[string]any{"desired_agent_version": "0.9.9"})
	if resp.StatusCode != 200 {
		t.Fatalf("partial patch: %d", resp.StatusCode)
	}
	resp.Body.Close()
	d, err := e.st.GetDevice(ctx, "win-zeal")
	if err != nil {
		t.Fatal(err)
	}
	if d.MAC != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("mac wiped: %q", d.MAC)
	}
	if !d.NormallyOff {
		t.Errorf("normally_off wiped")
	}
	if d.Schedule.GracePeriodS != 120 || len(d.Schedule.ExpectedOffline) != 1 {
		t.Errorf("schedule wiped: %+v", d.Schedule)
	}
	if !d.TerminalEnabled {
		t.Errorf("terminal_enabled wiped")
	}
	if d.DesiredAgentVersion != "0.9.9" {
		t.Errorf("desired_agent_version = %q, want 0.9.9", d.DesiredAgentVersion)
	}

	// The device_update audit row names what changed; this one changed only
	// the desired version via a separate row, so the settings row is empty.
	entries, _ := e.st.ListAudit(ctx, 10, 0)
	var sawUpdate, sawVersion bool
	for _, en := range entries {
		if en.Action == "device_update" {
			sawUpdate = true
			if en.Detail != "" {
				t.Errorf("device_update detail = %q, want empty (nothing settings-level changed)", en.Detail)
			}
		}
		if en.Action == "set_desired_version" && en.Detail == "0.9.9" {
			sawVersion = true
		}
	}
	if !sawUpdate || !sawVersion {
		t.Errorf("missing audit rows: update=%v version=%v", sawUpdate, sawVersion)
	}
}

func TestPatchSingleFieldChangesOnlyThatField(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	ctx := context.Background()
	if _, _, err := e.st.CreateDevice(ctx, "macos-desktop", "aa:bb:cc:dd:ee:ff", true, store.Schedule{GracePeriodS: 60}); err != nil {
		t.Fatal(err)
	}
	resp := e.do(t, "PATCH", "/api/devices/macos-desktop", map[string]any{"mac": "11:22:33:44:55:66"})
	if resp.StatusCode != 200 {
		t.Fatalf("patch mac: %d", resp.StatusCode)
	}
	resp.Body.Close()
	d, _ := e.st.GetDevice(ctx, "macos-desktop")
	if d.MAC != "11:22:33:44:55:66" {
		t.Errorf("mac not changed: %q", d.MAC)
	}
	if !d.NormallyOff || d.Schedule.GracePeriodS != 60 {
		t.Errorf("other fields changed: %+v", d)
	}
	entries, _ := e.st.ListAudit(ctx, 5, 0)
	found := false
	for _, en := range entries {
		if en.Action == "device_update" {
			found = true
			if en.Detail != "mac" {
				t.Errorf("audit detail = %q, want \"mac\"", en.Detail)
			}
		}
	}
	if !found {
		t.Error("no device_update audit row")
	}
}

func TestPatchEmptyBodyChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	ctx := context.Background()
	sched := store.Schedule{
		ExpectedOffline: []store.Window{{Days: "*", From: "02:00", To: "03:00"}},
		GracePeriodS:    45,
	}
	if _, _, err := e.st.CreateDevice(ctx, "pi", "aa:bb:cc:dd:ee:ff", false, sched); err != nil {
		t.Fatal(err)
	}
	for _, body := range []any{nil, map[string]any{}} {
		resp := e.do(t, "PATCH", "/api/devices/pi", body)
		if resp.StatusCode != 200 {
			t.Fatalf("empty patch (%v): %d", body, resp.StatusCode)
		}
		resp.Body.Close()
		d, _ := e.st.GetDevice(ctx, "pi")
		if d.MAC != "aa:bb:cc:dd:ee:ff" || d.NormallyOff || d.Schedule.GracePeriodS != 45 ||
			len(d.Schedule.ExpectedOffline) != 1 {
			t.Fatalf("empty patch changed device: %+v", d)
		}
	}
}

func TestPatchInvalidMacRejected(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	e.do(t, "POST", "/api/devices", map[string]any{"name": "deb", "mac": "aa:bb:cc:dd:ee:ff"})
	resp := e.do(t, "PATCH", "/api/devices/deb", map[string]any{"mac": "not-a-mac"})
	if resp.StatusCode != 400 {
		t.Fatalf("invalid mac: %d", resp.StatusCode)
	}
	resp.Body.Close()
	d, _ := e.st.GetDevice(context.Background(), "deb")
	if d.MAC != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("mac changed despite 400: %q", d.MAC)
	}
}

// POST register semantics must be unchanged: absent mac means "", invalid mac
// is 400.
func TestRegisterSemanticsUnchanged(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	ctx := context.Background()

	resp := e.do(t, "POST", "/api/devices", map[string]any{"name": "no-mac"})
	if resp.StatusCode != 201 {
		t.Fatalf("register without mac: %d", resp.StatusCode)
	}
	var reg struct {
		Device json.RawMessage `json:"device"`
		Token  string          `json:"token"`
	}
	json.NewDecoder(resp.Body).Decode(&reg)
	if reg.Token == "" {
		t.Error("no token returned")
	}
	d, err := e.st.GetDevice(ctx, "no-mac")
	if err != nil {
		t.Fatal(err)
	}
	if d.MAC != "" || d.NormallyOff {
		t.Errorf("register defaults changed: mac=%q normally_off=%v", d.MAC, d.NormallyOff)
	}

	resp = e.do(t, "POST", "/api/devices", map[string]any{"name": "bad-mac", "mac": "zz"})
	if resp.StatusCode != 400 {
		t.Fatalf("invalid mac on register: %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = e.do(t, "POST", "/api/devices", map[string]any{"name": "no-mac"})
	if resp.StatusCode != 409 {
		t.Fatalf("duplicate register: %d", resp.StatusCode)
	}
	resp.Body.Close()
}
