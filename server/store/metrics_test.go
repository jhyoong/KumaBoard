package store

import (
	"context"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
)

func f64(v float64) *float64 { return &v }

func TestRecordMetricsCoalescesPerBucket(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _, _ := s.CreateDevice(ctx, "deb", "", false, Schedule{})
	other, _, _ := s.CreateDevice(ctx, "pi", "", false, Schedule{})
	t0 := time.Unix(1_800_000_000, 0) // a multiple of 30
	// Two samples in the first bucket: the later one wins.
	s.RecordMetrics(ctx, d.ID, t0.Add(2*time.Second), proto.Metrics{CPUPercent: 10, MemUsedBytes: 1, MemTotalBytes: 4})
	if err := s.RecordMetrics(ctx, d.ID, t0.Add(29*time.Second), proto.Metrics{CPUPercent: 20, MemUsedBytes: 2, MemTotalBytes: 4}); err != nil {
		t.Fatal(err)
	}
	s.RecordMetrics(ctx, d.ID, t0.Add(30*time.Second), proto.Metrics{CPUPercent: 30, MemUsedBytes: 3, MemTotalBytes: 4,
		GPUs: []proto.GPU{{Index: 0, UtilPercent: f64(40)}, {Index: 1, UtilPercent: f64(70)}}})
	s.RecordMetrics(ctx, other.ID, t0, proto.Metrics{CPUPercent: 99})

	got, err := s.MetricsHistory(ctx, "deb", t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d samples, want 2: %+v", len(got), got)
	}
	if !got[0].Bucket.Equal(t0) || got[0].CPUPercent != 20 || got[0].MemUsedBytes != 2 || got[0].GPUPercent != nil {
		t.Fatalf("first bucket = %+v", got[0])
	}
	if !got[1].Bucket.Equal(t0.Add(30*time.Second)) || got[1].CPUPercent != 30 || got[1].GPUPercent == nil || *got[1].GPUPercent != 70 {
		t.Fatalf("second bucket = %+v", got[1])
	}
}

func TestMetricsHistoryWindow(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _, _ := s.CreateDevice(ctx, "deb", "", false, Schedule{})
	now := time.Unix(1_800_000_000, 0)
	for _, ago := range []time.Duration{90 * time.Minute, 61 * time.Minute, 59 * time.Minute, 0} {
		s.RecordMetrics(ctx, d.ID, now.Add(-ago), proto.Metrics{CPUPercent: ago.Minutes()})
	}
	got, err := s.MetricsHistory(ctx, "deb", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].CPUPercent != 59 || got[1].CPUPercent != 0 {
		t.Fatalf("window = %+v", got)
	}
	if got, _ := s.MetricsHistory(ctx, "missing", now.Add(-time.Hour)); got == nil || len(got) != 0 {
		t.Fatalf("unknown device = %#v, want empty non-nil", got)
	}
}

func TestDeleteMetricsBefore(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _, _ := s.CreateDevice(ctx, "deb", "", false, Schedule{})
	now := time.Unix(1_800_000_000, 0)
	for _, ago := range []time.Duration{3 * time.Hour, 2*time.Hour + time.Minute, time.Hour, 0} {
		s.RecordMetrics(ctx, d.ID, now.Add(-ago), proto.Metrics{CPUPercent: 1})
	}
	n, err := s.DeleteMetricsBefore(ctx, now.Add(-2*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("deleted %d err=%v, want 2", n, err)
	}
	got, _ := s.MetricsHistory(ctx, "deb", now.Add(-24*time.Hour))
	if len(got) != 2 {
		t.Fatalf("kept %d, want 2", len(got))
	}
}

func TestRollupMetrics(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _, _ := s.CreateDevice(ctx, "deb", "", false, Schedule{})
	other, _, _ := s.CreateDevice(ctx, "pi", "", false, Schedule{})
	now := time.Unix(1_800_000_000, 0) // a multiple of 900
	old := now.Add(-3 * time.Hour)     // a completed bucket past hot retention
	s.RecordMetrics(ctx, d.ID, old, proto.Metrics{CPUPercent: 10, MemUsedBytes: 100, MemTotalBytes: 400, DiskUsedPercent: 40})
	s.RecordMetrics(ctx, d.ID, old.Add(30*time.Second), proto.Metrics{CPUPercent: 30, MemUsedBytes: 300, MemTotalBytes: 400, DiskUsedPercent: 42,
		GPUs: []proto.GPU{{Index: 0, UtilPercent: f64(50)}}})
	s.RecordMetrics(ctx, other.ID, old, proto.Metrics{CPUPercent: 99})
	// A completed bucket within hot retention, and the bucket in progress.
	recent := now.Add(-30 * time.Minute)
	s.RecordMetrics(ctx, d.ID, recent, proto.Metrics{CPUPercent: 60, DiskUsedPercent: 45})
	s.RecordMetrics(ctx, d.ID, now.Add(time.Minute), proto.Metrics{CPUPercent: 80, DiskUsedPercent: 46})

	if err := s.RollupMetrics(ctx, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var rolled int
	s.db.QueryRow(`SELECT COUNT(*) FROM metrics_rollup WHERE device_id = ?`, d.ID).Scan(&rolled)
	if rolled != 2 {
		t.Fatalf("rolled %d buckets, want 2 (not the one in progress)", rolled)
	}
	// Only the hot tier's expired samples are dropped.
	if hot, _ := s.MetricsHistory(ctx, "deb", now.Add(-24*time.Hour)); len(hot) != 2 || hot[0].DiskUsedPercent != 45 {
		t.Fatalf("hot tier = %+v", hot)
	}

	got, err := s.MetricsRollupHistory(ctx, "deb", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("rollup history = %+v, want 3 buckets", got)
	}
	a := got[0]
	if !a.Bucket.Equal(old) || a.CPUPercent != 20 || a.MemUsedBytes != 200 || a.MemTotalBytes != 400 ||
		a.DiskUsedPercent != 41 || a.GPUPercent == nil || *a.GPUPercent != 50 {
		t.Fatalf("old bucket = %+v", a)
	}
	if b := got[1]; !b.Bucket.Equal(recent) || b.CPUPercent != 60 || b.GPUPercent != nil {
		t.Fatalf("recent bucket = %+v", b)
	}
	// The bucket in progress is averaged from the hot tier on the fly.
	if c := got[2]; !c.Bucket.Equal(now) || c.CPUPercent != 80 || c.DiskUsedPercent != 46 {
		t.Fatalf("live bucket = %+v", c)
	}

	// Rolling up again is idempotent: recomputed buckets are replaced, and
	// buckets whose samples are gone keep their stored rollup.
	if err := s.RollupMetrics(ctx, now.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	again, _ := s.MetricsRollupHistory(ctx, "deb", now.Add(-24*time.Hour))
	if len(again) != 3 || again[0].CPUPercent != 20 || again[2].CPUPercent != 80 {
		t.Fatalf("after second rollup = %+v", again)
	}
	if got, _ := s.MetricsRollupHistory(ctx, "missing", now.Add(-24*time.Hour)); got == nil || len(got) != 0 {
		t.Fatalf("unknown device = %#v, want empty non-nil", got)
	}
}

func TestMetricsRetentionAppliesToRollup(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _, _ := s.CreateDevice(ctx, "deb", "", false, Schedule{})
	now := time.Unix(1_800_000_000, 0)
	for _, ago := range []time.Duration{31 * 24 * time.Hour, 29 * 24 * time.Hour, 3 * time.Hour} {
		s.RecordMetrics(ctx, d.ID, now.Add(-ago), proto.Metrics{CPUPercent: 1})
	}
	if err := s.RollupMetrics(ctx, now); err != nil {
		t.Fatal(err)
	}
	n, err := s.DeleteMetricsBefore(ctx, now.Add(-MetricsRetention))
	if err != nil || n != 1 {
		t.Fatalf("deleted %d err=%v, want 1", n, err)
	}
	got, _ := s.MetricsRollupHistory(ctx, "deb", now.Add(-40*24*time.Hour))
	if len(got) != 2 || !got[0].Bucket.Equal(now.Add(-29*24*time.Hour)) {
		t.Fatalf("kept = %+v", got)
	}
}

func TestGPURollup(t *testing.T) {
	if gpuRollup(nil) != nil || gpuRollup([]proto.GPU{{Index: 0}}) != nil {
		t.Fatal("no reported utilisation must be nil")
	}
	if v := gpuRollup([]proto.GPU{{Index: 0, Suspended: true}}); v == nil || *v != 0 {
		t.Fatalf("suspended = %v, want 0", v)
	}
	if v := gpuRollup([]proto.GPU{{Index: 0, UtilPercent: f64(5)}, {Index: 1}, {Index: 2, UtilPercent: f64(9)}}); v == nil || *v != 9 {
		t.Fatalf("max = %v, want 9", v)
	}
}

func u64(v uint64) *uint64 { return &v }

func TestGPUMemRollup(t *testing.T) {
	if u, tot := gpuMemRollup(nil); u != nil || tot != nil {
		t.Fatalf("no GPUs = %v, %v", u, tot)
	}
	if u, tot := gpuMemRollup([]proto.GPU{{Index: 0, UtilPercent: f64(5)}}); u != nil || tot != nil {
		t.Fatalf("no memory reported = %v, %v", u, tot)
	}
	// Apple unified memory: used only, no total.
	if u, tot := gpuMemRollup([]proto.GPU{{Index: 0, MemUsedBytes: u64(7)}}); u == nil || *u != 7 || tot != nil {
		t.Fatalf("used only = %v, %v", u, tot)
	}
	// Summed across adapters, suspended included; adapters without memory skipped.
	u, tot := gpuMemRollup([]proto.GPU{
		{Index: 0, MemUsedBytes: u64(2), MemTotalBytes: u64(8)},
		{Index: 1, Suspended: true, MemUsedBytes: u64(1), MemTotalBytes: u64(4)},
		{Index: 2, UtilPercent: f64(9)},
	})
	if u == nil || *u != 3 || tot == nil || *tot != 12 {
		t.Fatalf("summed = %v, %v", u, tot)
	}
	// One adapter lacking a total makes the total unknown.
	if u, tot := gpuMemRollup([]proto.GPU{{Index: 0, MemUsedBytes: u64(2), MemTotalBytes: u64(8)}, {Index: 1, MemUsedBytes: u64(1)}}); u == nil || *u != 3 || tot != nil {
		t.Fatalf("partial totals = %v, %v", u, tot)
	}
}

// TestGPUMemoryHistory checks GPU memory is stored in the 30s tier, averaged
// into rollups (NULL samples skipped) and absent where nothing reported it.
func TestGPUMemoryHistory(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _, _ := s.CreateDevice(ctx, "deb", "", false, Schedule{})
	mac, _, _ := s.CreateDevice(ctx, "mac", "", false, Schedule{})
	now := time.Unix(1_800_000_000, 0)
	old := now.Add(-3 * time.Hour)
	gpu := func(used uint64) []proto.GPU {
		return []proto.GPU{{Index: 0, UtilPercent: f64(10), MemUsedBytes: u64(used), MemTotalBytes: u64(1000)}}
	}
	s.RecordMetrics(ctx, d.ID, old, proto.Metrics{GPUs: gpu(100)})
	s.RecordMetrics(ctx, d.ID, old.Add(30*time.Second), proto.Metrics{GPUs: gpu(300)})
	// A sample without GPU memory must not drag the average to zero.
	s.RecordMetrics(ctx, d.ID, old.Add(60*time.Second), proto.Metrics{GPUs: []proto.GPU{{Index: 0, UtilPercent: f64(10)}}})
	s.RecordMetrics(ctx, d.ID, now.Add(-30*time.Minute), proto.Metrics{CPUPercent: 1})
	s.RecordMetrics(ctx, mac.ID, old, proto.Metrics{GPUs: []proto.GPU{{Index: 0, MemUsedBytes: u64(5)}}})

	hot, _ := s.MetricsHistory(ctx, "deb", old)
	if len(hot) != 4 || hot[0].GPUMemUsedBytes == nil || *hot[0].GPUMemUsedBytes != 100 ||
		hot[0].GPUMemTotalBytes == nil || *hot[0].GPUMemTotalBytes != 1000 || hot[2].GPUMemUsedBytes != nil {
		t.Fatalf("hot tier = %+v", hot)
	}

	if err := s.RollupMetrics(ctx, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.MetricsRollupHistory(ctx, "deb", now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("rollup = %+v", got)
	}
	if a := got[0]; a.GPUMemUsedBytes == nil || *a.GPUMemUsedBytes != 200 || a.GPUMemTotalBytes == nil || *a.GPUMemTotalBytes != 1000 {
		t.Fatalf("old bucket = %+v", a)
	}
	if b := got[1]; b.GPUMemUsedBytes != nil || b.GPUMemTotalBytes != nil {
		t.Fatalf("bucket without GPU = %+v", b)
	}
	m, _ := s.MetricsRollupHistory(ctx, "mac", now.Add(-30*24*time.Hour))
	if len(m) != 1 || m[0].GPUMemUsedBytes == nil || *m[0].GPUMemUsedBytes != 5 || m[0].GPUMemTotalBytes != nil {
		t.Fatalf("mac rollup = %+v", m)
	}
}

// TestTempHistory checks host temperature is stored in the 30s tier, averaged
// into rollups (NULL samples skipped) and NULL where nothing reported it.
func TestTempHistory(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d, _, _ := s.CreateDevice(ctx, "deb", "", false, Schedule{})
	win, _, _ := s.CreateDevice(ctx, "win", "", false, Schedule{})
	now := time.Unix(1_800_000_000, 0)
	old := now.Add(-3 * time.Hour)
	s.RecordMetrics(ctx, d.ID, old, proto.Metrics{TempC: f64(50), TempSensor: "k10temp/Tctl"})
	s.RecordMetrics(ctx, d.ID, old.Add(30*time.Second), proto.Metrics{TempC: f64(60)})
	// A sample without a temperature must not drag the average to zero.
	s.RecordMetrics(ctx, d.ID, old.Add(60*time.Second), proto.Metrics{CPUPercent: 5})
	s.RecordMetrics(ctx, d.ID, now.Add(-30*time.Minute), proto.Metrics{CPUPercent: 1})
	s.RecordMetrics(ctx, win.ID, old, proto.Metrics{CPUPercent: 3})

	hot, err := s.MetricsHistory(ctx, "deb", old)
	if err != nil {
		t.Fatal(err)
	}
	if len(hot) != 4 || hot[0].TempC == nil || *hot[0].TempC != 50 || hot[1].TempC == nil || *hot[1].TempC != 60 ||
		hot[2].TempC != nil || hot[3].TempC != nil {
		t.Fatalf("hot tier = %+v", hot)
	}
	// A later sample in the same bucket replaces the temperature, including
	// replacing it with none.
	s.RecordMetrics(ctx, d.ID, old.Add(61*time.Second), proto.Metrics{TempC: f64(70)})
	s.RecordMetrics(ctx, d.ID, old.Add(62*time.Second), proto.Metrics{CPUPercent: 5})
	if hot, _ := s.MetricsHistory(ctx, "deb", old); hot[2].TempC != nil {
		t.Fatalf("conflict update kept temp: %+v", hot[2])
	}

	if err := s.RollupMetrics(ctx, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.MetricsRollupHistory(ctx, "deb", now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("rollup = %+v", got)
	}
	if a := got[0]; a.TempC == nil || *a.TempC != 55 {
		t.Fatalf("mixed bucket = %+v, want temp 55", a)
	}
	if b := got[1]; b.TempC != nil {
		t.Fatalf("bucket without temp = %+v", b)
	}
	w, _ := s.MetricsRollupHistory(ctx, "win", now.Add(-30*24*time.Hour))
	if len(w) != 1 || w[0].TempC != nil {
		t.Fatalf("all-nil rollup = %+v", w)
	}
}
