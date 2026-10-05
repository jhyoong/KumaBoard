//go:build !linux && !darwin && !windows

package collectors

// probeTemp finds nothing on platforms without a temperature backend.
func probeTemp(Options) tempSource { return nil }
