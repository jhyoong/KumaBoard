//go:build !linux && !darwin && !windows

package collectors

// probeGPUs finds nothing on platforms without a GPU backend.
func probeGPUs(Options) []GPUSource { return nil }
