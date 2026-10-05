package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/agent/app"
	"github.com/jhyoong/KumaBoard/agent/collectors"
	"github.com/jhyoong/KumaBoard/agent/upgrade"
	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/registry"
)

// fakeHwmon builds a /sys/class/hwmon equivalent with a k10temp chip at 54°C
// and a hotter NVMe chip that must be ignored, so the real Linux temperature
// backend runs on any host.
func fakeHwmon(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"hwmon0/name":        "nvme\n",
		"hwmon0/temp1_input": "70000\n",
		"hwmon0/temp1_label": "Composite\n",
		"hwmon1/name":        "k10temp\n",
		"hwmon1/temp1_input": "54000\n",
		"hwmon1/temp1_label": "Tctl\n",
		"hwmon1/temp3_input": "58000\n",
		"hwmon1/temp3_label": "Tccd1\n",
	}
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestTempMetricsEndToEnd runs a real agent whose Linux hwmon backend reads a
// fixture sysfs, and checks the host temperature reaches SSE, the metrics and
// device-list APIs, and history from both the 30s tier and the rollups.
func TestTempMetricsEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.feedRegistry()
	token := h.registerDevice("temp-box")
	cookie := h.loginCookie()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events := h.sseMetrics(ctx, cookie)

	cfg := h.agentConfig("temp-box", token)
	a := app.New(cfg, h.log, upgrade.StartupResult{})
	a.SetCollector(collectors.New(collectors.Options{HwmonRoot: fakeHwmon(t), ThermalRoot: t.TempDir(), Log: h.log}))
	agent := h.startApp(a, cfg, nil)

	data := nextSSEMetrics(t, events, func(d string) bool { return strings.Contains(d, `"device":"temp-box"`) })
	if !strings.Contains(data, `"temp_c":54,"temp_sensor":"k10temp/Tctl"`) {
		t.Fatalf("SSE metrics = %s", data)
	}
	var ev registry.MetricsEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Metrics.TempC == nil || *ev.Metrics.TempC != 54 || ev.Metrics.TempSensor != "k10temp/Tctl" {
		t.Fatalf("SSE metrics = %+v", ev.Metrics)
	}

	var hist []proto.Metrics
	if err := json.Unmarshal(h.apiMetrics(cookie, "temp-box"), &hist); err != nil {
		t.Fatal(err)
	}
	if len(hist) == 0 || hist[len(hist)-1].TempC == nil || *hist[len(hist)-1].TempC != 54 ||
		hist[len(hist)-1].TempSensor != "k10temp/Tctl" {
		t.Fatalf("API metrics = %+v", hist)
	}
	if list := h.apiGet(cookie, "/api/devices"); !strings.Contains(string(list), `"temp_c":54,"temp_sensor":"k10temp/Tctl"`) {
		t.Fatalf("device list = %s", list)
	}

	check := func(window string, step int64) {
		t.Helper()
		got := h.metricsHistory(t, cookie, "temp-box", window)
		if len(got.Samples) == 0 {
			t.Fatalf("%s: no samples", window)
		}
		for _, s := range got.Samples {
			if s.T%step != 0 || s.TempC == nil || *s.TempC != 54 {
				t.Fatalf("%s sample = %+v", window, s)
			}
		}
	}
	waitFor(t, 3*time.Second, func() bool { return h.events.count("metrics:temp-box") >= 2 })
	check("1h", 30)

	// Stop the agent and roll up as the prune loop would 3h from now, so the
	// long windows can only be answered from metrics_rollup.
	agent.cancel()
	time.Sleep(300 * time.Millisecond)
	if err := h.st.RollupMetrics(context.Background(), time.Now().Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"24h", "7d", "30d"} {
		check(w, 900)
	}
}

// TestSensorlessAgentOmitsTemp runs a real agent with no usable sensor (empty
// hwmon and thermal trees) and checks metrics still flow while temp_c and
// temp_sensor are absent end to end, never 0.
func TestSensorlessAgentOmitsTemp(t *testing.T) {
	h := newHarness(t)
	h.feedRegistry()
	token := h.registerDevice("cold")
	cookie := h.loginCookie()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events := h.sseMetrics(ctx, cookie)

	cfg := h.agentConfig("cold", token)
	a := app.New(cfg, h.log, upgrade.StartupResult{})
	a.SetCollector(collectors.New(collectors.Options{HwmonRoot: t.TempDir(), ThermalRoot: t.TempDir(), Log: h.log}))
	h.startApp(a, cfg, nil)

	data := nextSSEMetrics(t, events, func(d string) bool { return strings.Contains(d, `"device":"cold"`) })
	if strings.Contains(data, "temp_") || !strings.Contains(data, `"mem_total_bytes"`) {
		t.Fatalf("SSE metrics = %s", data)
	}
	waitFor(t, 3*time.Second, func() bool { return h.events.count("metrics:cold") >= 2 })
	for _, path := range []string{"/api/devices/cold/metrics", "/api/devices", "/api/devices/cold/metrics/history?window=1h"} {
		body := h.apiGet(cookie, path)
		if strings.Contains(string(body), "temp_") {
			t.Fatalf("%s carries temp: %s", path, body)
		}
	}
	if got := h.metricsHistory(t, cookie, "cold", "1h"); len(got.Samples) == 0 {
		t.Fatal("no history samples")
	}
}
