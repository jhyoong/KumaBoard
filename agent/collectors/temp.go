package collectors

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/sensors"
)

// tempTimeout caps one host temperature sample.
const tempTimeout = 2 * time.Second

// tempSource reads the host CPU/SoC temperature. Sample returns the reading in
// °C and a short label naming the sensor, or an error when no plausible
// reading was available this tick. It is called once per metrics tick, never
// concurrently.
type tempSource interface {
	Name() string
	Sample(ctx context.Context) (float64, string, error)
}

// tempCollector probes once, then samples the source every tick. A nil probe
// result, or gpuMaxFailures consecutive failed samples, disables temperature
// for the life of the process. It never produces an error for Collect.
type tempCollector struct {
	opts   Options
	probe  func(Options) tempSource
	probed bool
	src    tempSource
	gate   failGate
}

func newTempCollector(opts Options, probe func(Options) tempSource) *tempCollector {
	return &tempCollector{opts: opts, probe: probe}
}

// sample returns the temperature and sensor label, or nil when there is none.
func (t *tempCollector) sample(ctx context.Context) (*float64, string) {
	log := t.opts.Log
	if !t.probed {
		t.probed = true
		t.opts.ctx = ctx
		t.src = t.safeProbe()
		if t.src == nil {
			log.Info("temp: no sensor detected")
			return nil, ""
		}
		log.Info("temp: detected", "source", t.src.Name())
	}
	if t.src == nil || t.gate.disabled {
		return nil, ""
	}
	ctx, cancel := context.WithTimeout(ctx, tempTimeout)
	defer cancel()
	v, label, err := safeTempSample(ctx, t.src)
	if err == nil && !plausibleTemp(v) {
		err = fmt.Errorf("implausible reading %v°C from %s", v, label)
	}
	// failGate logs as a GPU helper; log the temperature variant here.
	t.gate.record(err, slog.New(slog.DiscardHandler), "")
	if t.gate.disabled {
		log.Warn("temp: disabled after repeated failures", "source", t.src.Name(), "err", err)
		t.src = nil
	}
	if err != nil {
		return nil, ""
	}
	return &v, label
}

// safeProbe keeps a panicking platform probe from taking the agent down.
func (t *tempCollector) safeProbe() (src tempSource) {
	defer func() {
		if r := recover(); r != nil {
			t.opts.Log.Warn("temp: probe panicked", "panic", r)
			src = nil
		}
	}()
	return t.probe(t.opts)
}

func safeTempSample(ctx context.Context, s tempSource) (v float64, label string, err error) {
	defer func() {
		if r := recover(); r != nil {
			v, label, err = 0, "", fmt.Errorf("panic: %v", r)
		}
	}()
	return s.Sample(ctx)
}

// plausibleTemp rejects readings no working CPU sensor produces: 0 and
// negatives (unset or broken sensors) and anything implausibly hot.
func plausibleTemp(v float64) bool { return v > 5 && v < 150 }

// ---- Linux hwmon / thermal_zone selection (pure) ----

// hwmonInput is one temp*_input of a hwmon chip, already in °C. Label is the
// temp*_label contents, or the input's base name ("temp1") without one.
type hwmonInput struct {
	Label string
	C     float64
}

// hwmonChip is one /sys/class/hwmon/hwmon* directory.
type hwmonChip struct {
	Name   string
	Inputs []hwmonInput
}

// hwmonExcludedNames and hwmonExcludedPrefixes are chips that are not the
// CPU/SoC: GPUs (covered by the GPU collector), disks, radios, DIMMs and power.
var (
	hwmonExcludedNames    = []string{"amdgpu", "nouveau", "nvidia", "radeon", "i915", "xe", "nvme", "drivetemp", "spd5118", "jc42"}
	hwmonExcludedPrefixes = []string{"iwlwifi", "mt7921", "ath", "brcmfmac", "BAT", "ucsi"}
)

func hwmonExcluded(name string) bool {
	for _, n := range hwmonExcludedNames {
		if name == n {
			return true
		}
	}
	for _, p := range hwmonExcludedPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// hwmonRank orders chips by how directly they measure the CPU/SoC. Higher is
// better; excluded chips are -1.
func hwmonRank(name string) int {
	switch {
	case hwmonExcluded(name):
		return -1
	case name == "coretemp", name == "k10temp", name == "zenpower":
		return 3
	case name == "cpu_thermal", name == "cpu-thermal", name == "soc_thermal":
		return 2
	case name == "acpitz":
		return 1
	}
	return 0
}

// pickHwmon chooses the host temperature from hwmon chips: the best-ranked
// chip with a plausible reading wins, and same-rank chips (dual-socket
// coretemp) contribute their maximum. The label is "<chip>/<input label>".
func pickHwmon(chips []hwmonChip) (float64, string, bool) {
	bestRank := -1
	var best float64
	var bestLabel string
	for _, c := range chips {
		r := hwmonRank(c.Name)
		if r < 0 || r < bestRank {
			continue
		}
		v, label, ok := chipTemp(c)
		if !ok {
			continue
		}
		if r > bestRank || v > best {
			bestRank, best, bestLabel = r, v, c.Name+"/"+label
		}
	}
	return best, bestLabel, bestRank >= 0
}

// chipTemp applies the per-driver input choice within one chip, ignoring
// implausible readings.
func chipTemp(c hwmonChip) (float64, string, bool) {
	var in []hwmonInput
	for _, i := range c.Inputs {
		if plausibleTemp(i.C) {
			in = append(in, i)
		}
	}
	switch c.Name {
	case "coretemp":
		// "Package id N" (N is the socket) is the package sensor.
		if v, l, ok := maxInput(in, func(l string) bool { return strings.HasPrefix(l, "Package id ") }); ok {
			return v, l, true
		}
		return maxInput(in, func(l string) bool { return strings.HasPrefix(l, "Core ") })
	case "k10temp":
		// Tdie, where present (older Ryzen/Threadripper), excludes the Tctl
		// offset. Tccd* are per-CCD and not the package reading.
		if v, l, ok := maxInput(in, func(l string) bool { return l == "Tdie" }); ok {
			return v, l, true
		}
		return maxInput(in, func(l string) bool { return l == "Tctl" })
	}
	return maxInput(in, func(string) bool { return true })
}

func maxInput(in []hwmonInput, match func(string) bool) (float64, string, bool) {
	var best float64
	var label string
	found := false
	for _, i := range in {
		if match(i.Label) && (!found || i.C > best) {
			best, label, found = i.C, i.Label, true
		}
	}
	return best, label, found
}

// thermalZone is one /sys/class/thermal/thermal_zone* directory.
type thermalZone struct {
	Dir  string // base name, e.g. "thermal_zone0"
	Type string
	C    float64
	OK   bool // whether temp was readable
}

// thermalZoneTypes are the CPU/SoC zone types, most preferred first.
var thermalZoneTypes = []string{"x86_pkg_temp", "cpu-thermal", "cpu_thermal", "soc_thermal"}

// pickThermalZone is the fallback when no hwmon chip qualifies: the first
// preferred zone type with a plausible reading. The label is "<type>/<zone>".
func pickThermalZone(zones []thermalZone) (float64, string, bool) {
	for _, typ := range thermalZoneTypes {
		for _, z := range zones {
			if z.Type == typ && z.OK && plausibleTemp(z.C) {
				return z.C, z.Type + "/" + z.Dir, true
			}
		}
	}
	return 0, "", false
}

// ---- Windows WMI thermal sources (pure) ----

// winTempReading is one thermal zone or probe, already in °C.
type winTempReading struct {
	Zone string
	C    float64
}

// winTempProbe is one WMI enumeration source. read returns every zone the
// source lists, or an error (commonly access denied for a non-admin service).
type winTempProbe struct {
	name string
	read func(ctx context.Context) ([]winTempReading, error)
}

// tenthsKelvinToC converts WMI's tenths-of-a-kelvin readings.
func tenthsKelvinToC(v float64) float64 { return v/10 - 273.15 }

func kelvinToC(v float64) float64 { return v - 273.15 }

// ambiguousTenthsToC converts a Win32_TemperatureProbe reading, which
// firmware reports in tenths of a kelvin or tenths of a degree Celsius. The
// plausible ranges of the two do not overlap, so kelvin is tried first.
func ambiguousTenthsToC(v float64) float64 {
	if c := tenthsKelvinToC(v); plausibleTemp(c) {
		return c
	}
	return v / 10
}

// thermalZoneInfoToC converts a "Thermal Zone Information" counter row:
// HighPrecisionTemperature (tenths of a kelvin) when populated, else
// Temperature (whole kelvin).
func thermalZoneInfoToC(highPrecision, kelvin uint32) float64 {
	if highPrecision > 0 {
		return tenthsKelvinToC(float64(highPrecision))
	}
	return kelvinToC(float64(kelvin))
}

// pickWinTemp takes the hottest plausible zone.
func pickWinTemp(rs []winTempReading) (float64, string, bool) {
	var best float64
	var zone string
	found := false
	for _, r := range rs {
		if plausibleTemp(r.C) && (!found || r.C > best) {
			best, zone, found = r.C, r.Zone, true
		}
	}
	return best, zone, found
}

// winTempSource samples the one WMI source chosen at probe time.
type winTempSource struct{ probe winTempProbe }

func (s *winTempSource) Name() string { return "windows-" + s.probe.name }

func (s *winTempSource) Sample(ctx context.Context) (float64, string, error) {
	rs, err := s.probe.read(ctx)
	if err != nil {
		return 0, "", err
	}
	v, zone, ok := pickWinTemp(rs)
	if !ok {
		return 0, "", fmt.Errorf("no plausible reading among %d zone(s)", len(rs))
	}
	if zone == "" {
		return v, s.probe.name, nil
	}
	return v, s.probe.name + "/" + zone, nil
}

// selectWinTemp returns the first source that yields a plausible reading. A
// source that errors (permissions, missing class) or lists nothing usable is
// recorded and the next one is tried.
func selectWinTemp(ctx context.Context, probes []winTempProbe) (tempSource, []probeAttempt) {
	var attempts []probeAttempt
	for _, p := range probes {
		s := &winTempSource{probe: p}
		if _, _, err := safeTempSample(ctx, s); err != nil {
			attempts = append(attempts, probeAttempt{Probe: p.name, Err: err})
			continue
		}
		return s, attempts
	}
	return nil, attempts
}

// ---- macOS sensor selection (pure) ----

// darwinIntelKeys are the SMC CPU proximity/die keys, in preference order.
var darwinIntelKeys = []string{"TC0P", "TC0D", "TC0E", "TC0F"}

// pickDarwin chooses the host temperature from gopsutil's macOS sensors.
// Apple Silicon: the hottest "PMU tdie*" CPU die sensor, else the hottest
// "PMU TP*" / pACC / eACC MTR sensor. Intel: the first SMC CPU key present.
// Readings failing plausibleTemp are ignored.
func pickDarwin(stats []sensors.TemperatureStat) (float64, string, bool) {
	hottest := func(match func(string) bool) (float64, string, bool) {
		var best float64
		var label string
		found := false
		for _, s := range stats {
			if match(s.SensorKey) && plausibleTemp(s.Temperature) && (!found || s.Temperature > best) {
				best, label, found = s.Temperature, s.SensorKey, true
			}
		}
		return best, label, found
	}
	if v, l, ok := hottest(func(k string) bool { return strings.HasPrefix(k, "PMU tdie") }); ok {
		return v, l, true
	}
	if v, l, ok := hottest(func(k string) bool {
		return strings.HasPrefix(k, "PMU TP") || strings.HasPrefix(k, "pACC MTR") || strings.HasPrefix(k, "eACC MTR")
	}); ok {
		return v, l, true
	}
	for _, key := range darwinIntelKeys {
		for _, s := range stats {
			if s.SensorKey == key && plausibleTemp(s.Temperature) {
				return s.Temperature, s.SensorKey, true
			}
		}
	}
	return 0, "", false
}
