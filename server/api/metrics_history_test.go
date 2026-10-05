package api

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

func TestMetricsHistory(t *testing.T) {
	e := newEnv(t)
	const path = "/api/devices/deb/metrics/history?window=1h"
	if resp := e.do(t, "GET", path, nil); resp.StatusCode != 401 {
		t.Fatalf("unauthenticated: %d", resp.StatusCode)
	}
	e.login(t)
	ctx := context.Background()
	d, _, err := e.st.CreateDevice(ctx, "deb", "", false, store.Schedule{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	util := 55.0
	e.st.RecordMetrics(ctx, d.ID, now.Add(-2*time.Hour), proto.Metrics{CPUPercent: 1, MemUsedBytes: 1, MemTotalBytes: 4})
	e.st.RecordMetrics(ctx, d.ID, now.Add(-time.Minute), proto.Metrics{CPUPercent: 12.5, MemUsedBytes: 1, MemTotalBytes: 4})
	used, total := uint64(3<<30), uint64(8<<30)
	e.st.RecordMetrics(ctx, d.ID, now, proto.Metrics{CPUPercent: 40, MemUsedBytes: 3, MemTotalBytes: 4,
		GPUs: []proto.GPU{{Index: 0, UtilPercent: &util, MemUsedBytes: &used, MemTotalBytes: &total}}})

	resp := e.do(t, "GET", path, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("history: %d", resp.StatusCode)
	}
	var got struct {
		Device  string                       `json:"device"`
		Window  string                       `json:"window"`
		Samples []map[string]json.RawMessage `json:"samples"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Device != "deb" || got.Window != "1h" || len(got.Samples) != 2 {
		t.Fatalf("history = %+v", got)
	}
	first, last := got.Samples[0], got.Samples[1]
	if string(first["cpu"]) != "12.5" || string(first["mem_pct"]) != "25" {
		t.Fatalf("first sample = %s %s", first["cpu"], first["mem_pct"])
	}
	for _, k := range []string{"gpu_pct", "gpu_mem_used_bytes", "gpu_mem_total_bytes"} {
		if _, ok := first[k]; ok {
			t.Fatalf("%s must be absent without a GPU", k)
		}
	}
	if string(last["gpu_pct"]) != "55" || string(last["mem_pct"]) != "75" {
		t.Fatalf("last sample = %s %s", last["gpu_pct"], last["mem_pct"])
	}
	if string(last["gpu_mem_used_bytes"]) != "3221225472" || string(last["gpu_mem_total_bytes"]) != "8589934592" {
		t.Fatalf("last gpu mem = %s / %s", last["gpu_mem_used_bytes"], last["gpu_mem_total_bytes"])
	}
	var t0, t1 int64
	json.Unmarshal(first["t"], &t0)
	json.Unmarshal(last["t"], &t1)
	if t0%30 != 0 || t1 <= t0 {
		t.Fatalf("timestamps %d, %d", t0, t1)
	}

	if string(first["disk_pct"]) != "0" {
		t.Fatalf("disk_pct = %s, want 0", first["disk_pct"])
	}

	for _, bad := range []string{"2h", "1H", "60m", "7D", "31d"} {
		if resp := e.do(t, "GET", "/api/devices/deb/metrics/history?window="+bad, nil); resp.StatusCode != 400 {
			t.Errorf("window=%s: %d, want 400", bad, resp.StatusCode)
		}
	}
	if resp := e.do(t, "GET", "/api/devices/nope/metrics/history?window=1h", nil); resp.StatusCode != 404 {
		t.Errorf("unknown device: %d, want 404", resp.StatusCode)
	}
	// The latest-sample endpoint is unaffected by the new route.
	if resp := e.do(t, "GET", "/api/devices/deb/metrics", nil); resp.StatusCode != 200 {
		t.Errorf("metrics: %d", resp.StatusCode)
	}
}

func TestMetricsHistoryLongWindows(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	ctx := context.Background()
	d, _, err := e.st.CreateDevice(ctx, "deb", "", false, store.Schedule{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	util := 30.0
	gpuUsed := uint64(1 << 30) // Apple-style: used memory but no total
	for _, ago := range []time.Duration{10 * 24 * time.Hour, 3 * 24 * time.Hour, 5 * time.Hour} {
		e.st.RecordMetrics(ctx, d.ID, now.Add(-ago), proto.Metrics{CPUPercent: 20, MemUsedBytes: 1, MemTotalBytes: 2,
			DiskUsedPercent: 61.5, GPUs: []proto.GPU{{Index: 0, UtilPercent: &util, MemUsedBytes: &gpuUsed}}})
	}
	if err := e.st.RollupMetrics(ctx, now); err != nil {
		t.Fatal(err)
	}
	// Recent samples still in the 30s tier are included too.
	e.st.RecordMetrics(ctx, d.ID, now, proto.Metrics{CPUPercent: 40, DiskUsedPercent: 62})

	for _, tc := range []struct {
		window string
		want   int
	}{{"24h", 2}, {"7d", 3}, {"30d", 4}} {
		resp := e.do(t, "GET", "/api/devices/deb/metrics/history?window="+tc.window, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", tc.window, resp.StatusCode)
		}
		var got struct {
			Window  string                       `json:"window"`
			Samples []map[string]json.RawMessage `json:"samples"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.Window != tc.window || len(got.Samples) != tc.want {
			t.Fatalf("%s: window=%s samples=%d, want %d", tc.window, got.Window, len(got.Samples), tc.want)
		}
		first, last := got.Samples[0], got.Samples[len(got.Samples)-1]
		var t0 int64
		json.Unmarshal(first["t"], &t0)
		if t0%900 != 0 || string(first["disk_pct"]) != "61.5" || string(first["mem_pct"]) != "50" || string(first["gpu_pct"]) != "30" {
			t.Fatalf("%s first = t:%d disk:%s mem:%s gpu:%s", tc.window, t0, first["disk_pct"], first["mem_pct"], first["gpu_pct"])
		}
		if string(last["cpu"]) != "40" || string(last["disk_pct"]) != "62" {
			t.Fatalf("%s last = cpu:%s disk:%s", tc.window, last["cpu"], last["disk_pct"])
		}
		if string(first["gpu_mem_used_bytes"]) != "1073741824" {
			t.Fatalf("%s first gpu mem = %s", tc.window, first["gpu_mem_used_bytes"])
		}
		if _, ok := first["gpu_mem_total_bytes"]; ok {
			t.Fatalf("%s: gpu_mem_total_bytes must be absent when unknown", tc.window)
		}
		for _, k := range []string{"gpu_pct", "gpu_mem_used_bytes", "gpu_mem_total_bytes"} {
			if _, ok := last[k]; ok {
				t.Fatalf("%s: %s must be absent without a GPU", tc.window, k)
			}
		}
	}
}

// TestMetricsTemp checks host temperature reaches the history and live
// metrics endpoints, and is omitted (never 0) where the device reported none.
func TestMetricsTemp(t *testing.T) {
	e := newEnv(t)
	e.login(t)
	ctx := context.Background()
	d, _, err := e.st.CreateDevice(ctx, "deb", "", false, store.Schedule{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.st.CreateDevice(ctx, "win", "", false, store.Schedule{}); err != nil {
		t.Fatal(err)
	}
	e.reg.DeviceAdded("deb")
	e.reg.DeviceAdded("win")
	now := time.Now()
	temp := 54.0
	e.st.RecordMetrics(ctx, d.ID, now.Add(-time.Minute), proto.Metrics{CPUPercent: 1})
	e.st.RecordMetrics(ctx, d.ID, now, proto.Metrics{CPUPercent: 2, TempC: &temp, TempSensor: "k10temp/Tctl"})

	for _, window := range []string{"1h", "24h"} {
		resp := e.do(t, "GET", "/api/devices/deb/metrics/history?window="+window, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", window, resp.StatusCode)
		}
		var got struct {
			Samples []map[string]json.RawMessage `json:"samples"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		last := got.Samples[len(got.Samples)-1]
		if string(last["temp_c"]) != "54" {
			t.Fatalf("%s last temp_c = %s, want 54", window, last["temp_c"])
		}
		if _, ok := last["temp_sensor"]; ok {
			t.Fatalf("%s: temp_sensor is live-only, not history", window)
		}
		if window == "1h" {
			if _, ok := got.Samples[0]["temp_c"]; ok {
				t.Fatalf("temp_c must be absent without a reading: %s", got.Samples[0]["temp_c"])
			}
		}
	}

	e.reg.MetricsReceived("deb", proto.Metrics{CPUPercent: 2, TempC: &temp, TempSensor: "k10temp/Tctl"})
	e.reg.MetricsReceived("win", proto.Metrics{CPUPercent: 3})
	for _, tc := range []struct{ path, want string }{
		{"/api/devices/deb/metrics", `"temp_c":54,"temp_sensor":"k10temp/Tctl"`},
		{"/api/devices", `"temp_c":54,"temp_sensor":"k10temp/Tctl"`},
	} {
		resp := e.do(t, "GET", tc.path, nil)
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || !strings.Contains(string(b), tc.want) {
			t.Fatalf("%s: %d %s", tc.path, resp.StatusCode, b)
		}
	}
	resp := e.do(t, "GET", "/api/devices/win/metrics", nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || strings.Contains(string(b), "temp_") {
		t.Fatalf("win metrics: %d %s", resp.StatusCode, b)
	}
}
