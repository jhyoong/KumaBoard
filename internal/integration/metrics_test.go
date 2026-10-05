package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestMetricsArriveOnInterval(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("a")
	h.startAgent(h.agentConfig("a", token), nil)
	waitFor(t, 3*time.Second, func() bool { return h.events.count("metrics:a") >= 3 })
}

type historyResp struct {
	Device  string `json:"device"`
	Window  string `json:"window"`
	Samples []struct {
		T       int64   `json:"t"`
		CPU     float64 `json:"cpu"`
		MemPct  float64 `json:"mem_pct"`
		DiskPct float64 `json:"disk_pct"`
		// Absent (nil) when no GPU reported memory / no total is known.
		GPUMemUsed  *uint64 `json:"gpu_mem_used_bytes"`
		GPUMemTotal *uint64 `json:"gpu_mem_total_bytes"`
		// Absent (nil) when the device reported no host temperature.
		TempC *float64 `json:"temp_c"`
	} `json:"samples"`
}

func (h *harness) metricsHistory(t *testing.T, cookie *http.Cookie, device, window string) historyResp {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/api/devices/"+device+"/metrics/history?window="+window, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history %s: %d", window, resp.StatusCode)
	}
	var got historyResp
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestMetricsHistoryFromAgent checks that samples from a real agent are
// persisted and served, coalesced to 30s buckets, by the history endpoint,
// and that long windows are served from 15-minute rollups.
func TestMetricsHistoryFromAgent(t *testing.T) {
	h := newHarness(t)
	token := h.registerDevice("a")
	cookie := h.loginCookie()
	agent := h.startAgent(h.agentConfig("a", token), nil)
	waitFor(t, 3*time.Second, func() bool { return h.events.count("metrics:a") >= 3 })

	got := h.metricsHistory(t, cookie, "a", "1h")
	// Three samples 200ms apart land in at most two 30s buckets.
	if got.Device != "a" || got.Window != "1h" || len(got.Samples) == 0 || len(got.Samples) > 2 {
		t.Fatalf("history = %+v", got)
	}
	last := got.Samples[len(got.Samples)-1]
	if last.T%30 != 0 || last.MemPct <= 0 || last.MemPct > 100 || last.DiskPct <= 0 || last.DiskPct > 100 {
		t.Fatalf("last sample = %+v", last)
	}
	// An agent without the gpu capability reports no GPU memory: no fake zeros.
	if last.GPUMemUsed != nil || last.GPUMemTotal != nil {
		t.Fatalf("GPU memory without a GPU: %+v", last)
	}

	// Stop the agent so no new samples land, then roll up as the prune loop
	// would 3h from now: every sample is rolled up and the 30s tier emptied,
	// so the 7d window can only be answered from metrics_rollup.
	agent.cancel()
	time.Sleep(300 * time.Millisecond) // let any in-flight sample land first
	if err := h.st.RollupMetrics(context.Background(), time.Now().Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if hot := h.metricsHistory(t, cookie, "a", "1h"); len(hot.Samples) != 0 {
		t.Fatalf("30s tier not emptied: %+v", hot)
	}
	week := h.metricsHistory(t, cookie, "a", "7d")
	if week.Window != "7d" || len(week.Samples) == 0 || len(week.Samples) > 2 {
		t.Fatalf("7d history = %+v", week)
	}
	for _, s := range week.Samples {
		if s.T%900 != 0 || s.MemPct <= 0 || s.DiskPct <= 0 {
			t.Fatalf("7d sample = %+v", s)
		}
	}
}
