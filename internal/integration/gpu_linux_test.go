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

// fakeAMDGPU builds a sysfs tree with one amdgpu card and returns its
// /sys/class/drm equivalent, so the real Linux backend runs on any host.
func fakeAMDGPU(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dev := filepath.Join(root, "devices", "0000:03:00.0")
	files := map[string]string{
		"vendor":                      "0x1002\n",
		"device":                      "0x73ff\n",
		"product_name":                "AMD Radeon RX 6600\n",
		"power/runtime_status":        "active\n",
		"gpu_busy_percent":            "37\n",
		"mem_info_vram_used":          "2254857830\n",
		"mem_info_vram_total":         "8573157376\n",
		"hwmon/hwmon3/temp1_input":    "61000\n",
		"hwmon/hwmon3/power1_average": "118000000\n",
	}
	for rel, content := range files {
		p := filepath.Join(dev, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../../bus/pci/drivers/amdgpu", filepath.Join(dev, "driver")); err != nil {
		t.Fatal(err)
	}
	card := filepath.Join(root, "drm", "card0")
	if err := os.MkdirAll(card, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../devices/0000:03:00.0", filepath.Join(card, "device")); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "drm")
}

// TestGPUMetricsEndToEnd runs a real agent whose Linux DRM backend reads a
// fixture sysfs, and checks the GPU sample reaches SSE and the metrics API.
func TestGPUMetricsEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.feedRegistry()
	token := h.registerDevice("gpu-box")
	cookie := h.loginCookie()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	events := h.sseMetrics(ctx, cookie)

	cfg := h.agentConfig("gpu-box", token)
	cfg.Capabilities = append(cfg.Capabilities, proto.CapGPU)
	a := app.New(cfg, h.log, upgrade.StartupResult{})
	a.SetCollector(collectors.New(collectors.Options{GPU: true, DRMRoot: fakeAMDGPU(t), Log: h.log}))
	h.startApp(a, cfg, nil)

	data := nextSSEMetrics(t, events, func(d string) bool {
		return strings.Contains(d, `"device":"gpu-box"`) && strings.Contains(d, `"gpus"`)
	})
	var ev registry.MetricsEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		t.Fatal(err)
	}
	checkFixtureGPU(t, "SSE", ev.Metrics.GPUs)

	var hist []proto.Metrics
	if err := json.Unmarshal(h.apiMetrics(cookie, "gpu-box"), &hist); err != nil {
		t.Fatal(err)
	}
	if len(hist) == 0 {
		t.Fatal("no metrics history")
	}
	checkFixtureGPU(t, "API", hist[len(hist)-1].GPUs)
}

func checkFixtureGPU(t *testing.T, where string, gpus []proto.GPU) {
	t.Helper()
	if len(gpus) != 1 {
		t.Fatalf("%s: gpus = %+v", where, gpus)
	}
	g := gpus[0]
	if g.Index != 0 || g.Name != "AMD Radeon RX 6600" || g.Vendor != "amd" || g.Suspended {
		t.Fatalf("%s: gpu = %+v", where, g)
	}
	if g.UtilPercent == nil || *g.UtilPercent != 37 ||
		g.MemUsedBytes == nil || *g.MemUsedBytes != 2254857830 ||
		g.MemTotalBytes == nil || *g.MemTotalBytes != 8573157376 ||
		g.TempC == nil || *g.TempC != 61 ||
		g.PowerW == nil || *g.PowerW != 118 {
		t.Fatalf("%s: gpu values wrong: %+v", where, g)
	}
}

// TestGPUMemoryHistoryEndToEnd runs a real agent with the fixture GPU and
// checks its VRAM usage is persisted and served by the history endpoint from
// both the 30s tier and the 15-minute rollups behind the 24h/7d/30d charts.
func TestGPUMemoryHistoryEndToEnd(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("gpu-box")
	cookie := h.loginCookie()

	cfg := h.agentConfig("gpu-box", token)
	cfg.Capabilities = append(cfg.Capabilities, proto.CapGPU)
	a := app.New(cfg, h.log, upgrade.StartupResult{})
	a.SetCollector(collectors.New(collectors.Options{GPU: true, DRMRoot: fakeAMDGPU(t), Log: h.log}))
	agent := h.startApp(a, cfg, nil)
	waitFor(t, 3*time.Second, func() bool { return h.events.count("metrics:gpu-box") >= 2 })

	check := func(window string, step int64) {
		t.Helper()
		got := h.metricsHistory(t, cookie, "gpu-box", window)
		if len(got.Samples) == 0 {
			t.Fatalf("%s: no samples", window)
		}
		for _, s := range got.Samples {
			if s.T%step != 0 || s.GPUMemUsed == nil || *s.GPUMemUsed != 2254857830 ||
				s.GPUMemTotal == nil || *s.GPUMemTotal != 8573157376 {
				t.Fatalf("%s sample = %+v", window, s)
			}
		}
	}
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
