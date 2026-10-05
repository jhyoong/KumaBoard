package api

import (
	"context"
	"errors"
	"testing"
)

func TestWakeDevice(t *testing.T) {
	var got []string
	e := newEnvWake(t, func(mac string) error {
		got = append(got, mac)
		return nil
	})
	e.login(t)
	e.do(t, "POST", "/api/devices", map[string]any{"name": "macos-desktop", "mac": "aa:bb:cc:dd:ee:ff"})
	e.do(t, "POST", "/api/devices", map[string]any{"name": "pi"})

	if resp := e.do(t, "POST", "/api/devices/macos-desktop/wake", nil); resp.StatusCode != 202 {
		t.Fatalf("wake: %d", resp.StatusCode)
	}
	if len(got) != 1 || got[0] != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("wake func got %v", got)
	}
	entries, _ := e.st.ListAudit(context.Background(), 1, 0)
	if len(entries) != 1 || entries[0].Action != "wake" {
		t.Fatalf("wake not audited: %+v", entries)
	}

	if resp := e.do(t, "POST", "/api/devices/pi/wake", nil); resp.StatusCode != 400 {
		t.Fatalf("no MAC: %d", resp.StatusCode)
	}
	if resp := e.do(t, "POST", "/api/devices/ghost/wake", nil); resp.StatusCode != 404 {
		t.Fatalf("unknown device: %d", resp.StatusCode)
	}
	// The dashboard used to POST here; there is no wake-by-MAC route.
	if resp := e.do(t, "POST", "/api/wake", map[string]string{"mac": "aa:bb:cc:dd:ee:ff"}); resp.StatusCode != 404 {
		t.Fatalf("/api/wake: %d", resp.StatusCode)
	}
	if len(got) != 1 {
		t.Fatalf("rejected requests reached the wake func: %v", got)
	}
}

func TestWakeDeviceSendFails(t *testing.T) {
	e := newEnvWake(t, func(string) error { return errors.New("network is unreachable") })
	e.login(t)
	e.do(t, "POST", "/api/devices", map[string]any{"name": "macos-desktop", "mac": "aa:bb:cc:dd:ee:ff"})
	if resp := e.do(t, "POST", "/api/devices/macos-desktop/wake", nil); resp.StatusCode != 500 {
		t.Fatalf("send failure: %d", resp.StatusCode)
	}
}

func TestWakeNotConfigured(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	e.do(t, "POST", "/api/devices", map[string]any{"name": "macos-desktop", "mac": "aa:bb:cc:dd:ee:ff"})
	if resp := e.do(t, "POST", "/api/devices/macos-desktop/wake", nil); resp.StatusCode != 503 {
		t.Fatalf("no sender: %d", resp.StatusCode)
	}
}
