package collectors

import (
	"context"
	"time"
)

// tempProbeTimeout caps each WMI source at probe time; the first COM
// round-trip after boot is slower than the per-tick ones. The probing tick's
// context bounds all of them together.
const tempProbeTimeout = 3 * time.Second

type wmiACPIThermalZone struct {
	InstanceName       string
	CurrentTemperature uint32 // tenths of a kelvin
}

type wmiTemperatureProbe struct {
	Name           string
	DeviceID       string
	CurrentReading int32 // tenths of a degree; null on most firmware
}

type wmiThermalZoneInfo struct {
	Name                     string
	Temperature              uint32 // kelvin
	HighPrecisionTemperature uint32 // tenths of a kelvin
}

// winTempProbes are the driver-free WMI sources, in preference order. None is
// a CPU package sensor (that needs a ring-0 driver); they are ACPI thermal
// zones, so the label names the source and zone.
//
//   - MSAcpi_ThermalZoneTemperature (root\WMI) usually needs administrator
//     rights.
//   - Win32_TemperatureProbe (root\cimv2) is readable by anyone, but SMBIOS
//     rarely populates CurrentReading.
//   - The "Thermal Zone Information" performance counters expose the same
//     ACPI zones to non-admin accounts.
func winTempProbes() []winTempProbe {
	return []winTempProbe{
		{name: "MSAcpi_ThermalZoneTemperature", read: func(ctx context.Context) ([]winTempReading, error) {
			var rows []wmiACPIThermalZone
			if err := wmiQuery(ctx, `SELECT InstanceName, CurrentTemperature FROM MSAcpi_ThermalZoneTemperature`, &rows, wmiNamespaceWMI); err != nil {
				return nil, err
			}
			out := make([]winTempReading, 0, len(rows))
			for _, r := range rows {
				out = append(out, winTempReading{Zone: r.InstanceName, C: tenthsKelvinToC(float64(r.CurrentTemperature))})
			}
			return out, nil
		}},
		{name: "Win32_TemperatureProbe", read: func(ctx context.Context) ([]winTempReading, error) {
			var rows []wmiTemperatureProbe
			if err := wmiQuery(ctx, `SELECT Name, DeviceID, CurrentReading FROM Win32_TemperatureProbe`, &rows, wmiNamespaceCIMv2); err != nil {
				return nil, err
			}
			out := make([]winTempReading, 0, len(rows))
			for _, r := range rows {
				zone := r.DeviceID
				if zone == "" {
					zone = r.Name
				}
				out = append(out, winTempReading{Zone: zone, C: ambiguousTenthsToC(float64(r.CurrentReading))})
			}
			return out, nil
		}},
		{name: "ThermalZoneInformation", read: func(ctx context.Context) ([]winTempReading, error) {
			var rows []wmiThermalZoneInfo
			if err := wmiQuery(ctx, `SELECT * FROM Win32_PerfFormattedData_Counters_ThermalZoneInformation`, &rows, wmiNamespaceCIMv2); err != nil {
				return nil, err
			}
			out := make([]winTempReading, 0, len(rows))
			for _, r := range rows {
				out = append(out, winTempReading{Zone: r.Name, C: thermalZoneInfoToC(r.HighPrecisionTemperature, r.Temperature)})
			}
			return out, nil
		}},
	}
}

// probeTemp walks winTempProbes and keeps the first source with a plausible
// reading. When none works it logs each attempt and its failure, once.
func probeTemp(opts Options) tempSource {
	var src tempSource
	var attempts []probeAttempt
	for _, p := range winTempProbes() {
		ctx, cancel := opts.probeContext(tempProbeTimeout)
		var a []probeAttempt
		src, a = selectWinTemp(ctx, []winTempProbe{p})
		cancel()
		attempts = append(attempts, a...)
		if src != nil {
			break
		}
	}
	if src == nil {
		opts.Log.Warn("temp: every probe failed", "attempts", attemptStrings(attempts))
	}
	return src
}
