package collectors

import (
	"context"
	"errors"
	"fmt"

	"github.com/shirou/gopsutil/v4/sensors"
)

// darwinTempSource reads gopsutil's unprivileged, cgo-free sensors: IOHID
// through purego on Apple Silicon, SMC through IOKit on Intel. The IOHID route
// is a private API, so reads are panic-guarded and bounded by ctx.
type darwinTempSource struct{}

func probeTemp(Options) tempSource {
	s := darwinTempSource{}
	ctx, cancel := context.WithTimeout(context.Background(), tempTimeout)
	defer cancel()
	if _, _, err := s.Sample(ctx); err != nil {
		return nil
	}
	return s
}

func (darwinTempSource) Name() string { return "darwin-sensors" }

func (darwinTempSource) Sample(ctx context.Context) (float64, string, error) {
	type result struct {
		stats []sensors.TemperatureStat
		err   error
	}
	// gopsutil ignores ctx, so the read runs on its own goroutine and a hang
	// is abandoned (the failGate disables the source after repeated ones).
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- result{err: fmt.Errorf("panic: %v", r)}
			}
		}()
		stats, err := sensors.TemperaturesWithContext(ctx)
		ch <- result{stats, err}
	}()
	select {
	case <-ctx.Done():
		return 0, "", ctx.Err()
	case r := <-ch:
		if r.err != nil && len(r.stats) == 0 {
			return 0, "", r.err
		}
		if v, label, ok := pickDarwin(r.stats); ok {
			return v, label, nil
		}
		return 0, "", errors.New("no plausible CPU temperature sensor")
	}
}
