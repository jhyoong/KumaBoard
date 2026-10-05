// Package collectors samples CPU, memory, disk, uptime, load, temperature,
// and GPUs.
package collectors

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"

	"github.com/jhyoong/KumaBoard/proto"
)

// Options configures a Collector.
type Options struct {
	// GPU enables GPU sampling (the gpu capability).
	GPU bool
	// DRMRoot replaces /sys/class/drm on Linux. Tests point it at a fixture.
	DRMRoot string
	// GPUSource replaces the platform GPU probe entirely.
	GPUSource GPUSource
	// HwmonRoot replaces /sys/class/hwmon on Linux. Tests point it at a fixture.
	HwmonRoot string
	// ThermalRoot replaces /sys/class/thermal on Linux, the fallback when no
	// hwmon chip qualifies.
	ThermalRoot string
	Log         *slog.Logger

	// ctx is the Collect context of the tick that runs the probe, so a slow
	// probe cannot outlast its caller (the upgrade selftest allows 5 s).
	ctx context.Context
}

// probeContext bounds one probe step by d and by the probing tick's context.
func (o Options) probeContext(d time.Duration) (context.Context, context.CancelFunc) {
	parent := o.ctx
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, d)
}

// Collector takes samples. It is stateful because some GPU backends keep a
// handle open across ticks or derive utilisation from the previous sample.
type Collector struct {
	mu   sync.Mutex
	temp *tempCollector
	gpu  *gpuCollector
}

// New creates a collector. The temperature and GPU probes run lazily on the
// first Collect.
func New(opts Options) *Collector {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	c := &Collector{temp: newTempCollector(opts, probeTemp)}
	if opts.GPU {
		probe := probeGPUs
		if opts.GPUSource != nil {
			probe = func(Options) []GPUSource { return []GPUSource{opts.GPUSource} }
		}
		c.gpu = newGPUCollector(opts, probe)
	}
	return c
}

// Collect takes one sample. Individual collectors that fail leave their fields
// zero rather than failing the whole sample; an error is returned only when
// nothing could be read. Temperature and GPU sampling are best-effort, never
// count toward that, and never cause an error.
func (c *Collector) Collect(ctx context.Context) (proto.Metrics, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var m proto.Metrics
	okCount := 0
	// Interval 0 measures since the previous call, which is the metrics interval.
	if pct, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(pct) > 0 {
		m.CPUPercent = pct[0]
		okCount++
	}
	if v, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		m.MemTotalBytes, m.MemUsedBytes, m.MemUsedPercent = v.Total, v.Used, v.UsedPercent
		okCount++
	}
	if u, err := disk.UsageWithContext(ctx, rootPath); err == nil {
		m.DiskTotalBytes, m.DiskUsedBytes, m.DiskUsedPercent = u.Total, u.Used, u.UsedPercent
		okCount++
	}
	if up, err := host.UptimeWithContext(ctx); err == nil {
		m.UptimeS = up
		okCount++
	}
	if l, err := load.AvgWithContext(ctx); err == nil {
		m.Load1, m.Load5, m.Load15 = l.Load1, l.Load5, l.Load15
	}
	if okCount == 0 {
		return m, errNoCollectors
	}
	m.TempC, m.TempSensor = c.temp.sample(ctx)
	// GPU last: nvidia-smi may be slow and must not delay the host sample.
	if c.gpu != nil {
		m.GPUs = c.gpu.sample(ctx)
	}
	return m, nil
}
