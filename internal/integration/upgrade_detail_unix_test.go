//go:build !windows

package integration

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/agent/upgrade"
	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

// TestUpgradeSelftestDetailReachesAPI runs a whole failed upgrade: a real
// server, a real agent, and a signed "release" that is a shell script acting
// as a target binary that rejects the device's config. What the script prints
// must reach the dashboard API. Unix only: the artifact is a #! script.
func TestUpgradeSelftestDetailReachesAPI(t *testing.T) {
	h := newHarness(t)
	const name, version = "selftest-detail", "9.9.9"
	const complaint = `config: command "backup": run must be a non-empty argv array`

	priv := h.useTestReleaseKey()
	stderrLine := "error: " + upgrade.SelftestConfigStage + " " + complaint
	script := "#!/bin/sh\n" +
		"echo checking config\n" +
		"echo '" + stderrLine + "' >&2\n" +
		"exit 1\n"
	h.publishRelease(version, runtime.GOOS, runtime.GOARCH, []byte(script), priv)

	token := h.registerDevice(name)
	// Set while the device is offline; the handshake then offers it.
	if d := h.patchTarget(name, version); d.UpgradeDispatch == nil || d.UpgradeDispatch.Code != store.DispatchNotConnected {
		t.Fatalf("offline PATCH dispatch = %+v, want %s", d.UpgradeDispatch, store.DispatchNotConnected)
	}

	a, bin := h.realUpgradeAgent(name, token)
	h.startApp(a, h.agentConfig(name, token), nil)

	waitFor(t, 20*time.Second, func() bool {
		lu := h.apiDevice(name).LatestUpgrade
		return lu != nil && lu.State == proto.UpgradeFailed
	})
	d := h.apiDevice(name)
	if d.UpgradeDispatch == nil || d.UpgradeDispatch.Code != store.DispatchSent || d.UpgradeDispatch.UpgradeID != d.LatestUpgrade.ID {
		t.Fatalf("dispatch = %+v, want sent for upgrade %s", d.UpgradeDispatch, d.LatestUpgrade.ID)
	}
	if d.LatestUpgrade.FailureReason != proto.UpgradeReasonSelftestConfigRejected {
		t.Fatalf("summary reason = %q, want %q", d.LatestUpgrade.FailureReason, proto.UpgradeReasonSelftestConfigRejected)
	}

	u := h.apiUpgrade(name, d.LatestUpgrade.ID)
	if u.State != proto.UpgradeFailed || u.FailureReason != proto.UpgradeReasonSelftestConfigRejected || u.ClosedBy != store.UpgradeSourceAgent {
		t.Fatalf("upgrade = state %q reason %q closed_by %q\ndetail: %s", u.State, u.FailureReason, u.ClosedBy, u.FailureDetail)
	}
	if u.ToVersion != version || u.FinishedAt == nil || u.Stalled {
		t.Fatalf("upgrade to_version=%q finished_at=%v stalled=%v", u.ToVersion, u.FinishedAt, u.Stalled)
	}
	for _, want := range []string{
		stderrLine,
		"selftest of " + version + " exited 1 after ",
		"--- stderr (last 4 KiB) ---",
		"checking config", // the child's stdout
	} {
		if !strings.Contains(u.FailureDetail, want) {
			t.Fatalf("detail lacks %q:\n%s", want, u.FailureDetail)
		}
	}
	if strings.Contains(u.FailureDetail, token) {
		t.Fatalf("detail leaks the device token:\n%s", u.FailureDetail)
	}

	assertEventStates(t, u.Events,
		proto.UpgradeRequested, proto.UpgradeDownloading, proto.UpgradeVerifying, proto.UpgradeSelftest, proto.UpgradeFailed)
	for i, e := range u.Events {
		wantSource := store.UpgradeSourceAgent
		if i == 0 {
			wantSource = store.UpgradeSourceServer
		}
		if e.Source != wantSource || !e.Applied {
			t.Fatalf("event %d (%s): source=%q applied=%v", i, e.State, e.Source, e.Applied)
		}
	}
	last := u.Events[len(u.Events)-1]
	if last.Reason != proto.UpgradeReasonSelftestConfigRejected || last.Detail != u.FailureDetail {
		t.Fatalf("final event = %+v", last)
	}

	// The device was not changed, and the failure freed the fleet slot.
	assertNotSwapped(t, bin)
	h.assertSlotFree()
}
