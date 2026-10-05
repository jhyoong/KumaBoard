package collectors

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/shirou/gopsutil/v4/sensors"
)

func TestPickHwmon(t *testing.T) {
	in := func(label string, c float64) hwmonInput { return hwmonInput{Label: label, C: c} }
	cases := []struct {
		name      string
		chips     []hwmonChip
		want      float64
		wantLabel string
		wantOK    bool
	}{
		{"coretemp package over cores", []hwmonChip{
			{Name: "coretemp", Inputs: []hwmonInput{in("Package id 0", 55), in("Core 0", 60), in("Core 1", 58)}},
		}, 55, "coretemp/Package id 0", true},
		{"coretemp cores without package", []hwmonChip{
			{Name: "coretemp", Inputs: []hwmonInput{in("Core 0", 51), in("Core 1", 58)}},
		}, 58, "coretemp/Core 1", true},
		{"k10temp prefers Tdie", []hwmonChip{
			{Name: "k10temp", Inputs: []hwmonInput{in("Tctl", 74), in("Tdie", 47), in("Tccd1", 80)}},
		}, 47, "k10temp/Tdie", true},
		{"k10temp Tctl ignores Tccd", []hwmonChip{
			{Name: "k10temp", Inputs: []hwmonInput{in("Tctl", 54), in("Tccd1", 61), in("Tccd2", 59)}},
		}, 54, "k10temp/Tctl", true},
		{"cpu_thermal only", []hwmonChip{
			{Name: "cpu_thermal", Inputs: []hwmonInput{in("temp1", 48.3)}},
		}, 48.3, "cpu_thermal/temp1", true},
		{"cpu beats hotter acpitz and unknown", []hwmonChip{
			{Name: "acpitz", Inputs: []hwmonInput{in("temp1", 90)}},
			{Name: "nct6775", Inputs: []hwmonInput{in("SYSTIN", 95)}},
			{Name: "k10temp", Inputs: []hwmonInput{in("Tctl", 40)}},
		}, 40, "k10temp/Tctl", true},
		{"acpitz beats unknown", []hwmonChip{
			{Name: "nct6775", Inputs: []hwmonInput{in("SYSTIN", 95)}},
			{Name: "acpitz", Inputs: []hwmonInput{in("temp1", 30)}},
		}, 30, "acpitz/temp1", true},
		{"gpu and nvme only", []hwmonChip{
			{Name: "amdgpu", Inputs: []hwmonInput{in("edge", 60)}},
			{Name: "nvme", Inputs: []hwmonInput{in("Composite", 70)}},
			{Name: "iwlwifi_1", Inputs: []hwmonInput{in("temp1", 45)}},
			{Name: "BAT0", Inputs: []hwmonInput{in("temp1", 30)}},
		}, 0, "", false},
		{"dual coretemp takes max", []hwmonChip{
			{Name: "coretemp", Inputs: []hwmonInput{in("Package id 0", 55), in("Core 0", 57)}},
			{Name: "coretemp", Inputs: []hwmonInput{in("Package id 1", 63), in("Core 0", 65)}},
		}, 63, "coretemp/Package id 1", true},
		{"implausible values dropped", []hwmonChip{
			{Name: "k10temp", Inputs: []hwmonInput{in("Tctl", 0)}},
			{Name: "cpu_thermal", Inputs: []hwmonInput{in("temp1", -3), in("temp2", 200)}},
			{Name: "acpitz", Inputs: []hwmonInput{in("temp1", 27.8)}},
		}, 27.8, "acpitz/temp1", true},
		{"all implausible", []hwmonChip{
			{Name: "coretemp", Inputs: []hwmonInput{in("Package id 0", 0), in("Core 0", -1), in("Core 1", 200)}},
		}, 0, "", false},
		{"no chips", nil, 0, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, label, ok := pickHwmon(c.chips)
			if ok != c.wantOK || v != c.want || label != c.wantLabel {
				t.Fatalf("got %v %q %v, want %v %q %v", v, label, ok, c.want, c.wantLabel, c.wantOK)
			}
		})
	}
}

func TestPickThermalZone(t *testing.T) {
	zones := []thermalZone{
		{Dir: "thermal_zone0", Type: "acpitz", C: 27.8, OK: true},
		{Dir: "thermal_zone1", Type: "cpu-thermal", C: 0, OK: true},
		{Dir: "thermal_zone2", Type: "soc_thermal", C: 44, OK: true},
		{Dir: "thermal_zone3", Type: "x86_pkg_temp", OK: false},
	}
	if v, label, ok := pickThermalZone(zones); !ok || v != 44 || label != "soc_thermal/thermal_zone2" {
		t.Fatalf("got %v %q %v", v, label, ok)
	}
	zones[3] = thermalZone{Dir: "thermal_zone3", Type: "x86_pkg_temp", C: 52, OK: true}
	if v, label, ok := pickThermalZone(zones); !ok || v != 52 || label != "x86_pkg_temp/thermal_zone3" {
		t.Fatalf("got %v %q %v", v, label, ok)
	}
	if _, _, ok := pickThermalZone(zones[:1]); ok {
		t.Fatal("acpitz is not a fallback zone")
	}
}

func TestPickDarwin(t *testing.T) {
	st := func(k string, c float64) sensors.TemperatureStat {
		return sensors.TemperatureStat{SensorKey: k, Temperature: c}
	}
	cases := []struct {
		name      string
		stats     []sensors.TemperatureStat
		want      float64
		wantLabel string
		wantOK    bool
	}{
		{"apple silicon die", []sensors.TemperatureStat{
			st("PMU tdie1", 41), st("PMU tdie3", 47.5), st("PMU TP1s", 60), st("NAND CH0 temp", 70), st("gas gauge battery", 30),
		}, 47.5, "PMU tdie3", true},
		{"apple silicon MTR fallback", []sensors.TemperatureStat{
			st("pACC MTR Temp Sensor2", 52), st("eACC MTR Temp Sensor0", 45), st("PMU TP3w", 50), st("PMU tdie1", 0),
		}, 52, "pACC MTR Temp Sensor2", true},
		{"intel smc", []sensors.TemperatureStat{
			st("TA0P", 30), st("TC0D", 66), st("TC0P", 0), st("TG0D", 70),
		}, 66, "TC0D", true},
		{"intel smc proximity first", []sensors.TemperatureStat{
			st("TC0D", 66), st("TC0P", 58),
		}, 58, "TC0P", true},
		{"nothing usable", []sensors.TemperatureStat{
			st("NAND CH0 temp", 40), st("TA0P", 30), st("TC0P", 0),
		}, 0, "", false},
		{"empty", nil, 0, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, label, ok := pickDarwin(c.stats)
			if ok != c.wantOK || v != c.want || label != c.wantLabel {
				t.Fatalf("got %v %q %v, want %v %q %v", v, label, ok, c.want, c.wantLabel, c.wantOK)
			}
		})
	}
}

func TestPlausibleTemp(t *testing.T) {
	for v, want := range map[float64]bool{-10: false, 0: false, 5: false, 5.1: true, 54: true, 149.9: true, 150: false, 200: false} {
		if plausibleTemp(v) != want {
			t.Errorf("plausibleTemp(%v) = %v", v, !want)
		}
	}
}

type fakeTempSource struct {
	v     float64
	label string
	err   error
	panic bool
	calls int
}

func (f *fakeTempSource) Name() string { return "fake" }

func (f *fakeTempSource) Sample(context.Context) (float64, string, error) {
	f.calls++
	if f.panic {
		panic("boom")
	}
	return f.v, f.label, f.err
}

func TestTempProbeOnceNone(t *testing.T) {
	probes := 0
	tc := newTempCollector(testOpts(), func(Options) tempSource { probes++; return nil })
	for range 3 {
		if v, label := tc.sample(context.Background()); v != nil || label != "" {
			t.Fatalf("got %v %q", v, label)
		}
	}
	if probes != 1 {
		t.Fatalf("probed %d times", probes)
	}
}

func TestTempSample(t *testing.T) {
	src := &fakeTempSource{v: 54, label: "k10temp/Tctl"}
	tc := newTempCollector(testOpts(), func(Options) tempSource { return src })
	v, label := tc.sample(context.Background())
	if v == nil || *v != 54 || label != "k10temp/Tctl" {
		t.Fatalf("got %v %q", v, label)
	}
}

func TestTempDisabledAfterFailures(t *testing.T) {
	src := &fakeTempSource{err: errors.New("broken")}
	tc := newTempCollector(testOpts(), func(Options) tempSource { return src })
	for range 5 {
		if v, _ := tc.sample(context.Background()); v != nil {
			t.Fatalf("got %v", *v)
		}
	}
	if src.calls != gpuMaxFailures {
		t.Fatalf("sampled %d times, want %d", src.calls, gpuMaxFailures)
	}
}

func TestTempImplausibleCountsAsFailure(t *testing.T) {
	src := &fakeTempSource{v: 0, label: "x"}
	tc := newTempCollector(testOpts(), func(Options) tempSource { return src })
	for range 5 {
		if v, _ := tc.sample(context.Background()); v != nil {
			t.Fatalf("implausible reading returned: %v", *v)
		}
	}
	if src.calls != gpuMaxFailures {
		t.Fatalf("sampled %d times, want %d", src.calls, gpuMaxFailures)
	}
}

func TestTempFailuresResetOnSuccess(t *testing.T) {
	src := &fakeTempSource{err: errors.New("flaky")}
	tc := newTempCollector(testOpts(), func(Options) tempSource { return src })
	tc.sample(context.Background())
	tc.sample(context.Background())
	src.err, src.v, src.label = nil, 40, "x"
	if v, _ := tc.sample(context.Background()); v == nil || *v != 40 {
		t.Fatalf("got %v", v)
	}
	src.err = errors.New("flaky")
	tc.sample(context.Background())
	tc.sample(context.Background())
	if tc.gate.disabled {
		t.Fatal("non-consecutive failures must not disable")
	}
}

func TestTempPanicIsIsolated(t *testing.T) {
	tc := newTempCollector(testOpts(), func(Options) tempSource { return &fakeTempSource{panic: true} })
	if v, _ := tc.sample(context.Background()); v != nil {
		t.Fatalf("got %v", *v)
	}
	tc2 := newTempCollector(testOpts(), func(Options) tempSource { panic("probe boom") })
	if v, _ := tc2.sample(context.Background()); v != nil {
		t.Fatalf("got %v", *v)
	}
}

func TestCollectNeverFailsOnTemp(t *testing.T) {
	for _, src := range []*fakeTempSource{{err: errors.New("broken")}, {panic: true}, {v: 500}} {
		c := New(Options{})
		c.temp = newTempCollector(testOpts(), func(Options) tempSource { return src })
		m, err := c.Collect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if m.TempC != nil || m.TempSensor != "" || m.MemTotalBytes == 0 {
			t.Fatalf("got %+v", m)
		}
	}
}

func nearC(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestWinTempConversions(t *testing.T) {
	cases := []struct {
		name      string
		got, want float64
	}{
		{"tenths 3182", tenthsKelvinToC(3182), 45.05},
		{"tenths 2732", tenthsKelvinToC(2732), 0.05},
		{"tenths 0", tenthsKelvinToC(0), -273.15},
		{"kelvin 318", kelvinToC(318), 44.85},
		{"kelvin 273.15", kelvinToC(273.15), 0},
		{"kelvin 0", kelvinToC(0), -273.15},
		{"ambiguous tenths kelvin", ambiguousTenthsToC(3182), 45.05},
		{"ambiguous tenths celsius", ambiguousTenthsToC(450), 45},
		{"ambiguous zero", ambiguousTenthsToC(0), 0},
		{"ambiguous hot celsius", ambiguousTenthsToC(1050), 105},
		{"ambiguous cool kelvin", ambiguousTenthsToC(2932), 20.05},
		{"zone high precision preferred", thermalZoneInfoToC(3182, 300), 45.05},
		{"zone whole kelvin fallback", thermalZoneInfoToC(0, 318), 44.85},
		{"zone unset", thermalZoneInfoToC(0, 0), -273.15},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !nearC(c.got, c.want) {
				t.Fatalf("got %v, want %v", c.got, c.want)
			}
		})
	}
}

func TestPickWinTemp(t *testing.T) {
	cases := []struct {
		name     string
		rs       []winTempReading
		want     float64
		wantZone string
		wantOK   bool
	}{
		{"hottest plausible", []winTempReading{
			{"TZ00", 40}, {"TZ01", 55.5}, {"TZ02", 48},
		}, 55.5, "TZ01", true},
		{"implausible dropped", []winTempReading{
			{"zero kelvin", kelvinToC(0)}, {"TZ00", 41}, {"celsius as kelvin", tenthsKelvinToC(27315)}, {"zero", 0}, {"hot", 150},
		}, 41, "TZ00", true},
		{"first of equals", []winTempReading{{"a", 50}, {"b", 50}}, 50, "a", true},
		{"unnamed zone", []winTempReading{{"", 45}}, 45, "", true},
		{"all implausible", []winTempReading{{"TZ00", -273.15}, {"TZ01", 2458.35}, {"TZ02", 5}}, 0, "", false},
		{"none", nil, 0, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, zone, ok := pickWinTemp(c.rs)
			if ok != c.wantOK || !nearC(v, c.want) || zone != c.wantZone {
				t.Fatalf("got %v %q %v, want %v %q %v", v, zone, ok, c.want, c.wantZone, c.wantOK)
			}
		})
	}
}

// winProbe builds a probe returning fixed readings and counting its reads.
func winProbe(name string, calls *int, rs []winTempReading, err error) winTempProbe {
	return winTempProbe{name: name, read: func(context.Context) ([]winTempReading, error) {
		*calls++
		return rs, err
	}}
}

func TestWinTempSource(t *testing.T) {
	denied := errors.New("Access denied")
	cases := []struct {
		name      string
		rs        []winTempReading
		err       error
		want      float64
		wantLabel string
		wantErr   string
	}{
		{"probe/zone label", []winTempReading{{`ACPI\ThermalZone\TZ00_0`, 45.05}, {"cold", 30}}, nil, 45.05, `acpi-tz/ACPI\ThermalZone\TZ00_0`, ""},
		{"probe name when zone empty", []winTempReading{{"", 45}}, nil, 45, "acpi-tz", ""},
		{"read error", []winTempReading{{"TZ00", 45}}, denied, 0, "", "Access denied"},
		{"no plausible reading", []winTempReading{{"TZ00", -273.15}, {"TZ01", 0}}, nil, 0, "", "no plausible reading among 2 zone(s)"},
		{"no zones", nil, nil, 0, "", "no plausible reading among 0 zone(s)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls := 0
			s := &winTempSource{probe: winProbe("acpi-tz", &calls, c.rs, c.err)}
			if s.Name() != "windows-acpi-tz" {
				t.Fatalf("Name = %q", s.Name())
			}
			v, label, err := s.Sample(context.Background())
			if c.wantErr != "" {
				if err == nil || err.Error() != c.wantErr || v != 0 || label != "" {
					t.Fatalf("got %v %q %v, want error %q", v, label, err, c.wantErr)
				}
				if c.err != nil && !errors.Is(err, c.err) {
					t.Fatalf("read error not propagated: %v", err)
				}
				return
			}
			if err != nil || !nearC(v, c.want) || label != c.wantLabel {
				t.Fatalf("got %v %q %v, want %v %q", v, label, err, c.want, c.wantLabel)
			}
			if calls != 1 {
				t.Fatalf("read %d times", calls)
			}
		})
	}
}

func TestSelectWinTemp(t *testing.T) {
	denied := errors.New("Access denied")
	good := []winTempReading{{"TZ00", 45.05}}
	bad := []winTempReading{{"TZ00", -273.15}, {"TZ01", 2458.35}}
	panicProbe := func(calls *int) winTempProbe {
		return winTempProbe{name: "panicky", read: func(context.Context) ([]winTempReading, error) {
			*calls++
			panic("boom")
		}}
	}

	t.Run("first works, rest not called", func(t *testing.T) {
		var a, b, c int
		src, attempts := selectWinTemp(context.Background(), []winTempProbe{
			winProbe("a", &a, good, nil), winProbe("b", &b, good, nil), winProbe("c", &c, nil, denied),
		})
		if src == nil || src.Name() != "windows-a" || len(attempts) != 0 {
			t.Fatalf("got %v %v", src, attempts)
		}
		if a != 1 || b != 0 || c != 0 {
			t.Fatalf("calls = %d %d %d", a, b, c)
		}
		if v, label, err := src.Sample(context.Background()); err != nil || !nearC(v, 45.05) || label != "a/TZ00" {
			t.Fatalf("got %v %q %v", v, label, err)
		}
	})

	t.Run("permission error falls through", func(t *testing.T) {
		var a, b, c int
		src, attempts := selectWinTemp(context.Background(), []winTempProbe{
			winProbe("a", &a, nil, denied), winProbe("b", &b, good, nil), winProbe("c", &c, good, nil),
		})
		if src == nil || src.Name() != "windows-b" {
			t.Fatalf("got %v", src)
		}
		if len(attempts) != 1 || attempts[0].Probe != "a" || !errors.Is(attempts[0].Err, denied) {
			t.Fatalf("attempts = %v", attempts)
		}
		if a != 1 || b != 1 || c != 0 {
			t.Fatalf("calls = %d %d %d", a, b, c)
		}
	})

	t.Run("implausible readings fall through", func(t *testing.T) {
		var a, b int
		src, attempts := selectWinTemp(context.Background(), []winTempProbe{
			winProbe("a", &a, bad, nil), winProbe("b", &b, good, nil),
		})
		if src == nil || src.Name() != "windows-b" {
			t.Fatalf("got %v", src)
		}
		if got, want := attemptStrings(attempts), []string{"a: no plausible reading among 2 zone(s)"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("attempts = %q, want %q", got, want)
		}
	})

	t.Run("all fail", func(t *testing.T) {
		var a, b, c, d int
		src, attempts := selectWinTemp(context.Background(), []winTempProbe{
			winProbe("a", &a, nil, denied), winProbe("b", &b, bad, nil), panicProbe(&c), winProbe("d", &d, nil, nil),
		})
		if src != nil {
			t.Fatalf("got source %v", src.Name())
		}
		want := []string{
			"a: Access denied",
			"b: no plausible reading among 2 zone(s)",
			"panicky: panic: boom",
			"d: no plausible reading among 0 zone(s)",
		}
		if got := attemptStrings(attempts); !reflect.DeepEqual(got, want) {
			t.Fatalf("attempts = %q, want %q", got, want)
		}
		if a != 1 || b != 1 || c != 1 || d != 1 {
			t.Fatalf("calls = %d %d %d %d", a, b, c, d)
		}
	})

	t.Run("panic is recorded, next tried", func(t *testing.T) {
		var a, b int
		src, attempts := selectWinTemp(context.Background(), []winTempProbe{panicProbe(&a), winProbe("b", &b, good, nil)})
		if src == nil || src.Name() != "windows-b" {
			t.Fatalf("got %v", src)
		}
		if len(attempts) != 1 || attempts[0].Probe != "panicky" || !strings.Contains(attempts[0].Err.Error(), "boom") {
			t.Fatalf("attempts = %v", attempts)
		}
	})

	t.Run("no probes", func(t *testing.T) {
		if src, attempts := selectWinTemp(context.Background(), nil); src != nil || len(attempts) != 0 {
			t.Fatalf("got %v %v", src, attempts)
		}
	})
}
