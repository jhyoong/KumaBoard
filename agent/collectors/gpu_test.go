package collectors

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
)

// nvidia-smi --query-gpu=...,pci.bus_id --format=csv,noheader,nounits from a
// desktop with two cards, the second reporting no power sensor.
const nvidiaSMIFixture = `0, NVIDIA GeForce RTX 3080, 37, 2145, 10240, 61, 118.52, 00000000:01:00.0
1, NVIDIA GeForce GT 1030, 0, 5, 2048, 34, [N/A], 00000000:02:00.0
`

func TestParseNvidiaSMI(t *testing.T) {
	rows, err := parseNvidiaSMI([]byte(nvidiaSMIFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	r := rows[0]
	if r.Index != 0 || r.Name != "NVIDIA GeForce RTX 3080" || r.BusID != "0000:01:00.0" {
		t.Fatalf("row 0 = %+v", r)
	}
	if *r.Util != 37 || *r.MemUsed != 2145<<20 || *r.MemTotal != 10240<<20 || *r.TempC != 61 || *r.PowerW != 118.52 {
		t.Fatalf("row 0 values wrong: util=%v used=%v total=%v temp=%v power=%v", *r.Util, *r.MemUsed, *r.MemTotal, *r.TempC, *r.PowerW)
	}
	if rows[1].PowerW != nil {
		t.Fatalf("[N/A] power must be absent, got %v", *rows[1].PowerW)
	}
}

func TestParseNvidiaSMIRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver.\n", "x, y, 1, 2, 3, 4, 5, 00000000:01:00.0\n"} {
		if _, err := parseNvidiaSMI([]byte(in)); err == nil {
			t.Fatalf("expected error for %q", in)
		}
	}
}

func TestNormalizeBusID(t *testing.T) {
	if got := normalizeBusID("00000000:0A:00.0"); got != "0000:0a:00.0" {
		t.Fatal(got)
	}
}

func TestResidencyBusy(t *testing.T) {
	// 30 s tick, GPU idle (in RC6) for 21 s of it: 30% busy.
	if u, ok := residencyBusy(1000, 22000, 30*time.Second); !ok || u < 29.99 || u > 30.01 {
		t.Fatalf("got %v %v", u, ok)
	}
	if _, ok := residencyBusy(5000, 100, 30*time.Second); ok {
		t.Fatal("counter reset must be discarded")
	}
	if u, _ := residencyBusy(0, 40000, 30*time.Second); u != 0 {
		t.Fatalf("idle > wall must clamp to 0, got %v", u)
	}
}

// PDH "\GPU Engine(*)\Utilization Percentage" and "\GPU Adapter Memory(*)\…"
// instance shapes from a Windows host with a dGPU (…C9E1) and an iGPU (…D2A7).
var (
	pdhEngines = map[string]float64{
		"pid_1234_luid_0x00000000_0x0000C9E1_phys_0_eng_0_engtype_3D":          30,
		"pid_5678_luid_0x00000000_0x0000C9E1_phys_0_eng_0_engtype_3D":          25,
		"pid_1234_luid_0x00000000_0x0000C9E1_phys_0_eng_3_engtype_VideoDecode": 40,
		"pid_9012_luid_0x00000000_0x0000C9E1_phys_0_eng_5_engtype_Compute_0":   80,
		"pid_9012_luid_0x00000000_0x0000C9E1_phys_0_eng_6_engtype_Compute_0":   45,
		"pid_4321_luid_0x00000000_0x0000D2A7_phys_0_eng_0_engtype_3D":          7,
		"pid_4321_luid_0x00000000_0x0000D2A7_phys_0_eng_1_engtype_Copy":        2,
	}
	pdhDedicated = map[string]float64{
		"luid_0x00000000_0x0000C9E1_phys_0": 2254857830,
		"luid_0x00000000_0x0000D2A7_phys_0": 0,
		"luid_0x00000000_0x0000FFFF_phys_0": 0, // idle software adapter
	}
	pdhShared = map[string]float64{
		"luid_0x00000000_0x0000C9E1_phys_0": 104857600,
		"luid_0x00000000_0x0000D2A7_phys_0": 402653184,
		"luid_0x00000000_0x0000FFFF_phys_0": 0,
	}
)

func TestPDHEngineInstance(t *testing.T) {
	luid, eng, ok := pdhEngineInstance("pid_1234_luid_0x00000000_0x0000C9E1_phys_0_eng_3_engtype_VideoDecode")
	if !ok || luid != "0x00000000_0x0000c9e1" || eng != "VideoDecode" {
		t.Fatalf("got %q %q %v", luid, eng, ok)
	}
	for _, bad := range []string{"_Total", "pid_1_luid_0x0_phys_0_eng_0_engtype_3D", "pid_1_luid_0x0_0x1_phys_0"} {
		if _, _, ok := pdhEngineInstance(bad); ok {
			t.Fatalf("%q must not parse", bad)
		}
	}
	if u := aggregateEngineUtil(map[string]float64{"_Total": 99}); len(u) != 0 {
		t.Fatalf("unparseable instances must be ignored: %v", u)
	}
}

func TestAggregateEngineUtil(t *testing.T) {
	u := aggregateEngineUtil(pdhEngines)
	// dGPU: 3D=55, VideoDecode=40, Compute_0=125→100; max is 100.
	if u["0x00000000_0x0000c9e1"] != 100 {
		t.Fatalf("dGPU util = %v", u["0x00000000_0x0000c9e1"])
	}
	if u["0x00000000_0x0000d2a7"] != 7 {
		t.Fatalf("iGPU util = %v", u["0x00000000_0x0000d2a7"])
	}
}

func TestBuildWindowsGPUsSingleAdapter(t *testing.T) {
	gpus := buildWindowsGPUs(map[string]float64{
		"pid_1_luid_0x00000000_0x0000C9E1_phys_0_eng_0_engtype_3D": 12.5,
	}, map[string]float64{"luid_0x00000000_0x0000C9E1_phys_0": 2254857830}, nil,
		[]winAdapter{{Name: "NVIDIA GeForce RTX 3080", VRAMBytes: 10737418240}})
	if len(gpus) != 1 {
		t.Fatalf("gpus = %+v", gpus)
	}
	g := gpus[0]
	if g.Name != "NVIDIA GeForce RTX 3080" || g.Vendor != "nvidia" || *g.UtilPercent != 12.5 ||
		*g.MemUsedBytes != 2254857830 || *g.MemTotalBytes != 10737418240 {
		t.Fatalf("got %+v", g)
	}
}

func TestBuildWindowsGPUsAmbiguousUsesLUID(t *testing.T) {
	gpus := buildWindowsGPUs(pdhEngines, pdhDedicated, pdhShared,
		[]winAdapter{{Name: "NVIDIA GeForce RTX 3080"}, {Name: "Intel(R) UHD Graphics 770"}})
	if len(gpus) != 2 {
		t.Fatalf("software adapter must be skipped: %+v", gpus)
	}
	d, i := gpus[0], gpus[1]
	if d.Index != 0 || d.Name != "GPU 0x00000000_0x0000c9e1" || d.Vendor != "unknown" || d.MemTotalBytes != nil {
		t.Fatalf("dGPU = %+v", d)
	}
	if *d.UtilPercent != 100 || *d.MemUsedBytes != 2254857830 {
		t.Fatalf("dGPU values = %v %v", *d.UtilPercent, *d.MemUsedBytes)
	}
	if i.Index != 1 || *i.UtilPercent != 7 || *i.MemUsedBytes != 402653184 {
		t.Fatalf("iGPU = %+v", i)
	}
}

func TestBuildWindowsGPUsFirstTickHasNoUtil(t *testing.T) {
	gpus := buildWindowsGPUs(nil, pdhDedicated, pdhShared, nil)
	if len(gpus) != 2 || gpus[0].UtilPercent != nil {
		t.Fatalf("got %+v", gpus)
	}
}

// Shape of `ioreg -r -d 1 -c IOAccelerator` on an M2 Max.
var ioregM2Max = map[string]int64{
	"Device Utilization %":   12,
	"Renderer Utilization %": 11,
	"Tiler Utilization %":    3,
	"In use system memory":   1234567890,
	"Alloc system memory":    2345678901,
	"recoveryCount":          0,
}

func TestAppleGPU(t *testing.T) {
	g := appleGPU(0, "Apple M2 Max", ioregM2Max)
	if g.Name != "Apple M2 Max" || g.Vendor != "apple" || *g.UtilPercent != 12 || *g.MemUsedBytes != 1234567890 {
		t.Fatalf("got %+v", g)
	}
	if g.MemTotalBytes != nil || g.TempC != nil || g.PowerW != nil {
		t.Fatalf("unified memory has no total, and v1 has no temp/power: %+v", g)
	}
	if g := appleGPU(1, "", map[string]int64{}); g.Name != "Apple GPU" || g.UtilPercent != nil {
		t.Fatalf("empty stats: %+v", g)
	}
}

func TestVendorHelpers(t *testing.T) {
	if vendorFromPCI("0x1002") != "amd" || vendorFromPCI("0x8086\n") != "intel" || vendorFromPCI("0x10DE") != "nvidia" || vendorFromPCI("0x1af4") != "unknown" {
		t.Fatal("vendorFromPCI")
	}
	if vendorFromName("AMD Radeon RX 6600") != "amd" || vendorFromName("Intel(R) Arc(TM) A380") != "intel" || vendorFromName("Apple M4") != "apple" {
		t.Fatal("vendorFromName")
	}
}

type fakeGPUSource struct {
	calls int
	err   error
	panic bool
}

func (f *fakeGPUSource) Name() string { return "fake" }

func (f *fakeGPUSource) Sample(context.Context) ([]proto.GPU, error) {
	f.calls++
	if f.panic {
		panic("boom")
	}
	if f.err != nil {
		return nil, f.err
	}
	return []proto.GPU{{Index: 0, Name: "Fake", Vendor: "unknown", UtilPercent: ptr(5.0)}}, nil
}

func testOpts() Options { return Options{Log: slog.New(slog.DiscardHandler)} }

func TestGPUProbeOnceNone(t *testing.T) {
	probes := 0
	g := newGPUCollector(testOpts(), func(Options) []GPUSource { probes++; return nil })
	for range 3 {
		if out := g.sample(context.Background()); out != nil {
			t.Fatalf("got %+v", out)
		}
	}
	if probes != 1 {
		t.Fatalf("probed %d times", probes)
	}
}

func TestGPUBackendDisabledAfterFailures(t *testing.T) {
	src := &fakeGPUSource{err: errors.New("broken")}
	g := newGPUCollector(testOpts(), func(Options) []GPUSource { return []GPUSource{src} })
	for range 5 {
		g.sample(context.Background())
	}
	if src.calls != gpuMaxFailures {
		t.Fatalf("sampled %d times, want %d", src.calls, gpuMaxFailures)
	}
}

func TestGPUFailuresResetOnSuccess(t *testing.T) {
	src := &fakeGPUSource{err: errors.New("flaky")}
	g := newGPUCollector(testOpts(), func(Options) []GPUSource { return []GPUSource{src} })
	g.sample(context.Background())
	g.sample(context.Background())
	src.err = nil
	if out := g.sample(context.Background()); len(out) != 1 {
		t.Fatalf("got %+v", out)
	}
	src.err = errors.New("flaky")
	g.sample(context.Background())
	g.sample(context.Background())
	if g.backends[0].disabled {
		t.Fatal("non-consecutive failures must not disable")
	}
}

func TestGPUPanicIsIsolated(t *testing.T) {
	src := &fakeGPUSource{panic: true}
	g := newGPUCollector(testOpts(), func(Options) []GPUSource { return []GPUSource{src} })
	if out := g.sample(context.Background()); out != nil {
		t.Fatalf("got %+v", out)
	}
	g2 := newGPUCollector(testOpts(), func(Options) []GPUSource { panic("probe boom") })
	if out := g2.sample(context.Background()); out != nil {
		t.Fatalf("got %+v", out)
	}
}

func TestCollectNeverFailsOnGPU(t *testing.T) {
	c := New(Options{GPU: true, GPUSource: &fakeGPUSource{err: errors.New("broken")}})
	m, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.GPUs != nil || m.MemTotalBytes == 0 {
		t.Fatalf("got %+v", m)
	}
}

func TestCollectWithoutGPUCapability(t *testing.T) {
	src := &fakeGPUSource{}
	m, err := New(Options{GPUSource: src}).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.GPUs != nil || src.calls != 0 {
		t.Fatalf("GPU sampled without capability: %+v", m)
	}
}

func TestParseNvidiaSMIWindowsWrapping(t *testing.T) {
	want, err := parseNvidiaSMI([]byte(nvidiaSMIFixture))
	if err != nil {
		t.Fatal(err)
	}
	crlf := strings.ReplaceAll(nvidiaSMIFixture, "\n", "\r\n")
	var nul strings.Builder
	for i := 0; i < len(nvidiaSMIFixture); i++ {
		nul.WriteByte(nvidiaSMIFixture[i])
		nul.WriteByte(0)
	}
	cases := []struct{ name, in string }{
		{"bom", "\xef\xbb\xbf" + nvidiaSMIFixture},
		{"crlf", crlf},
		{"bare cr", strings.ReplaceAll(nvidiaSMIFixture, "\n", "\r")},
		{"nul bytes", nul.String()},
		{"leading blank lines", "\n\r\n\n" + nvidiaSMIFixture},
		{"trailing blank line", nvidiaSMIFixture + "\n"},
		{"trailing blank crlf", crlf + "\r\n\r\n"},
		{"no final newline", strings.TrimSuffix(nvidiaSMIFixture, "\n")},
		{"everything", "\xef\xbb\xbf\x00\r\n\r\n" + crlf + "\x00\r\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseNvidiaSMI([]byte(c.in))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestCleanSMIOutput(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"empty", "", ""},
		{"plain", "a\nb\n", "a\nb\n"},
		{"bom", "\xef\xbb\xbfa\n", "a\n"},
		{"bom only at start", "a\xef\xbb\xbf\n", "a\xef\xbb\xbf\n"},
		{"crlf", "a\r\nb\r\n", "a\nb\n"},
		{"bare cr", "a\rb\r", "a\nb\n"},
		{"nul", "a\x00,\x00b\x00\n\x00", "a,b\n"},
		{"leading blank", "\r\n\n\ra\n", "a\n"},
		{"trailing blank kept", "a\n\n", "a\n\n"},
		{"only wrapping", "\xef\xbb\xbf\x00\r\n\r", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := string(cleanSMIOutput([]byte(c.in))); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseNvidiaSMIRejectsWrappedGarbage(t *testing.T) {
	for _, in := range []string{
		"\xef\xbb\xbf",
		"\xef\xbb\xbf\r\n\r\n",
		"\x00\x00",
		"\xef\xbb\xbfNVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver.\r\n",
		"\r\nx, y, 1, 2, 3, 4, 5, 00000000:01:00.0\r\n",
		"0, NVIDIA GeForce RTX 3080, 37, 2145, 10240, 61, 118.52\r\n",
	} {
		if _, err := parseNvidiaSMI([]byte(in)); err == nil {
			t.Fatalf("expected error for %q", in)
		}
	}
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestNvidiaSMICandidates(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"empty env", nil, []string{
			`C:\Windows\System32\nvidia-smi.exe`,
			`C:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe`,
			`C:\Windows\System32\DriverStore\FileRepository\nv*\nvidia-smi.exe`,
		}},
		{"custom SystemRoot", map[string]string{"SystemRoot": `D:\WINNT`}, []string{
			`D:\WINNT\System32\nvidia-smi.exe`,
			`C:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe`,
			`D:\WINNT\System32\DriverStore\FileRepository\nv*\nvidia-smi.exe`,
		}},
		{"64-bit process", map[string]string{
			"SystemRoot": `C:\Windows`, "ProgramFiles": `C:\Program Files`,
			"ProgramW6432": `C:\Program Files`, "ProgramFiles(x86)": `C:\Program Files (x86)`,
		}, []string{
			`C:\Windows\System32\nvidia-smi.exe`,
			`C:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe`,
			`C:\Program Files (x86)\NVIDIA Corporation\NVSMI\nvidia-smi.exe`,
			`C:\Windows\System32\DriverStore\FileRepository\nv*\nvidia-smi.exe`,
		}},
		{"dedupe ignores case", map[string]string{
			"ProgramFiles": `c:\PROGRAM FILES`, "ProgramW6432": `C:\Program Files`,
		}, []string{
			`C:\Windows\System32\nvidia-smi.exe`,
			`c:\PROGRAM FILES\NVIDIA Corporation\NVSMI\nvidia-smi.exe`,
			`C:\Windows\System32\DriverStore\FileRepository\nv*\nvidia-smi.exe`,
		}},
		{"32-bit process", map[string]string{
			"ProgramFiles": `C:\Program Files (x86)`, "ProgramW6432": `C:\Program Files`,
			"ProgramFiles(x86)": `C:\Program Files (x86)`,
		}, []string{
			`C:\Windows\System32\nvidia-smi.exe`,
			`C:\Program Files (x86)\NVIDIA Corporation\NVSMI\nvidia-smi.exe`,
			`C:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe`,
			`C:\Windows\System32\DriverStore\FileRepository\nv*\nvidia-smi.exe`,
		}},
		{"fallback when not covered", map[string]string{"ProgramFiles": `D:\Apps`}, []string{
			`C:\Windows\System32\nvidia-smi.exe`,
			`D:\Apps\NVIDIA Corporation\NVSMI\nvidia-smi.exe`,
			`C:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe`,
			`C:\Windows\System32\DriverStore\FileRepository\nv*\nvidia-smi.exe`,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nvidiaSMICandidates(envOf(c.env))
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got\n%q\nwant\n%q", got, c.want)
			}
			for _, p := range got {
				if strings.Contains(p, "/") {
					t.Fatalf("forward slash in %q", p)
				}
			}
		})
	}
}

func TestResolveNvidiaSMI(t *testing.T) {
	const (
		sys32  = `C:\Windows\System32\nvidia-smi.exe`
		nvsmi  = `C:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe`
		store  = `C:\Windows\System32\DriverStore\FileRepository\nv*\nvidia-smi.exe`
		storeA = `C:\Windows\System32\DriverStore\FileRepository\nv_dispi.inf_amd64_0a1b\nvidia-smi.exe`
		storeB = `C:\Windows\System32\DriverStore\FileRepository\nv_dispi.inf_amd64_9f8e\nvidia-smi.exe`
		onPath = `D:\tools\nvidia-smi.exe`
	)
	cases := []struct {
		name      string
		path      string
		pathErr   error
		exists    []string
		glob      []string
		want      string
		wantTried []string
	}{
		{"PATH hit wins", onPath, nil, []string{sys32, nvsmi}, []string{storeA}, onPath, []string{"PATH"}},
		{"PATH miss, System32", "", exec.ErrNotFound, []string{sys32, nvsmi}, []string{storeA}, sys32, []string{"PATH", sys32}},
		{"empty PATH result is a miss", "", nil, []string{sys32}, nil, sys32, []string{"PATH", sys32}},
		{"NVSMI dir", "", exec.ErrNotFound, []string{nvsmi}, []string{storeA}, nvsmi, []string{"PATH", sys32, nvsmi}},
		{"DriverStore single", "", exec.ErrNotFound, nil, []string{storeA}, storeA, []string{"PATH", sys32, nvsmi, store}},
		{"DriverStore last sorted", "", exec.ErrNotFound, nil, []string{storeB, storeA}, storeB, []string{"PATH", sys32, nvsmi, store}},
		{"nothing found", "", exec.ErrNotFound, nil, nil, "", []string{"PATH", sys32, nvsmi, store}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var globbed []string
			got, tried := resolveNvidiaSMI(smiResolver{
				lookPath: func(name string) (string, error) {
					if name != "nvidia-smi.exe" {
						t.Fatalf("lookPath(%q)", name)
					}
					return c.path, c.pathErr
				},
				exists: func(p string) bool {
					for _, e := range c.exists {
						if e == p {
							return true
						}
					}
					return false
				},
				glob: func(pat string) []string {
					globbed = append(globbed, pat)
					return append([]string(nil), c.glob...)
				},
				getenv: envOf(nil),
			})
			if got != c.want || !reflect.DeepEqual(tried, c.wantTried) {
				t.Fatalf("got %q tried %q, want %q tried %q", got, tried, c.want, c.wantTried)
			}
			for _, g := range globbed {
				if g != store {
					t.Fatalf("globbed %q", g)
				}
			}
		})
	}
}

func TestResolveNvidiaSMITriedMatchesCandidates(t *testing.T) {
	env := envOf(map[string]string{
		"SystemRoot": `D:\WINNT`, "ProgramFiles": `D:\Apps`, "ProgramFiles(x86)": `D:\Apps (x86)`,
	})
	got, tried := resolveNvidiaSMI(smiResolver{
		lookPath: func(string) (string, error) { return "", exec.ErrNotFound },
		exists:   func(string) bool { return false },
		glob:     func(string) []string { return nil },
		getenv:   env,
	})
	if want := append([]string{"PATH"}, nvidiaSMICandidates(env)...); got != "" || !reflect.DeepEqual(tried, want) {
		t.Fatalf("got %q tried %q, want tried %q", got, tried, want)
	}
}

func TestGPUsFromNvidiaRows(t *testing.T) {
	rows, err := parseNvidiaSMI([]byte(nvidiaSMIFixture))
	if err != nil {
		t.Fatal(err)
	}
	gpus := gpusFromNvidiaRows(append(rows, nvidiaRow{Index: 2}))
	if len(gpus) != 3 {
		t.Fatalf("gpus = %+v", gpus)
	}
	g := gpus[0]
	if g.Index != 0 || g.Name != "NVIDIA GeForce RTX 3080" || g.Vendor != "nvidia" || *g.UtilPercent != 37 ||
		*g.MemUsedBytes != 2145<<20 || *g.MemTotalBytes != 10240<<20 || *g.TempC != 61 || *g.PowerW != 118.52 {
		t.Fatalf("got %+v", g)
	}
	if g := gpus[1]; g.Index != 1 || g.Name != "NVIDIA GeForce GT 1030" || g.PowerW != nil {
		t.Fatalf("got %+v", g)
	}
	if g := gpus[2]; g.Index != 2 || g.Name != "NVIDIA GPU" || g.Vendor != "nvidia" || g.UtilPercent != nil || g.MemTotalBytes != nil {
		t.Fatalf("nameless row: %+v", g)
	}
	if out := gpusFromNvidiaRows(nil); out == nil || len(out) != 0 {
		t.Fatalf("got %#v", out)
	}
}

func TestExecErrDetail(t *testing.T) {
	plain := errors.New("exec: not found")
	if got := execErrDetail(plain); got != plain {
		t.Fatalf("non-exit error changed: %v", got)
	}
	if got := execErrDetail(nil); got != nil {
		t.Fatalf("nil changed: %v", got)
	}
	for _, stderr := range []string{"", "\r\n \x00\n"} {
		ee := &exec.ExitError{Stderr: []byte(stderr)}
		if got := execErrDetail(ee); got != error(ee) {
			t.Fatalf("stderr %q: error changed: %v", stderr, got)
		}
	}
	cases := []struct{ name, stderr, want string }{
		{"one line", "driver not loaded\n", "driver not loaded"},
		{"first line only", "first\nsecond\n", "first"},
		{"windows wrapping", "\xef\xbb\xbf\r\n  Unable to determine the device handle\r\nsecond\r\n", "Unable to determine the device handle"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ee := &exec.ExitError{Stderr: []byte(c.stderr)}
			for _, in := range []error{ee, fmt.Errorf("run: %w", ee)} {
				got := execErrDetail(in)
				if got.Error() != in.Error()+": "+c.want {
					t.Fatalf("got %q, want %q", got, in.Error()+": "+c.want)
				}
				var as *exec.ExitError
				if !errors.As(got, &as) || as != ee {
					t.Fatalf("exit error not wrapped: %v", got)
				}
			}
		})
	}
}

func TestAttemptStrings(t *testing.T) {
	got := attemptStrings([]probeAttempt{
		{Probe: "nvidia-smi", Err: errors.New("not found")},
		{Probe: "pdh", Err: errors.New("access denied")},
	})
	if want := []string{"nvidia-smi: not found", "pdh: access denied"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := attemptStrings(nil); got == nil || len(got) != 0 {
		t.Fatalf("got %#v", got)
	}
}

func TestAdaptersFromWMI(t *testing.T) {
	cases := []struct {
		name     string
		rows     []wmiVideoController
		registry []winAdapter
		want     []winAdapter
	}{
		{"none", nil, nil, nil},
		{"software and empty skipped", []wmiVideoController{
			{Name: "Microsoft Basic Display Adapter", AdapterRAM: 1 << 20},
			{Name: "", AdapterCompatibility: "NVIDIA", AdapterRAM: 1 << 30},
			{Name: "  \t", PNPDeviceID: `PCI\VEN_10DE&DEV_2206`},
			{Name: "Microsoft Remote Display Adapter"},
		}, nil, nil},
		{"vendor from name", []wmiVideoController{
			{Name: " NVIDIA GeForce RTX 3080 ", AdapterCompatibility: "Intel Corporation", PNPDeviceID: `PCI\VEN_1002&DEV_73FF`, AdapterRAM: 1 << 30},
		}, nil, []winAdapter{{Name: "NVIDIA GeForce RTX 3080", Vendor: "nvidia", VRAMBytes: 1 << 30}}},
		{"vendor from AdapterCompatibility", []wmiVideoController{
			{Name: "UHD Graphics 770", AdapterCompatibility: "Intel Corporation", PNPDeviceID: `PCI\VEN_10DE&DEV_2206`},
		}, nil, []winAdapter{{Name: "UHD Graphics 770", Vendor: "intel"}}},
		{"vendor from PNPDeviceID", []wmiVideoController{
			{Name: "Video Controller", AdapterCompatibility: "(Standard display types)", PNPDeviceID: `PCI\VEN_10DE&DEV_2206&SUBSYS_38971462&REV_01\4&2D78AB8F&0&0008`},
			{Name: "Video Controller B", PNPDeviceID: `pci\ven_1002&dev_73ff`},
			{Name: "Video Controller C", PNPDeviceID: `PCI\VEN_8086`},
		}, nil, []winAdapter{
			{Name: "Video Controller", Vendor: "nvidia"},
			{Name: "Video Controller B", Vendor: "amd"},
			{Name: "Video Controller C", Vendor: "intel"},
		}},
		{"vendor unknown", []wmiVideoController{
			{Name: "Video Controller", PNPDeviceID: `PCI\VEN_1AF4&DEV_1050`},
			{Name: "Truncated", PNPDeviceID: `PCI\VEN_10`},
			{Name: "No ID"},
		}, nil, []winAdapter{
			{Name: "Video Controller", Vendor: "unknown"},
			{Name: "Truncated", Vendor: "unknown"},
			{Name: "No ID", Vendor: "unknown"},
		}},
		{"saturated AdapterRAM dropped", []wmiVideoController{
			{Name: "NVIDIA A", AdapterRAM: 0xFFF00000},
			{Name: "NVIDIA B", AdapterRAM: 0xFFFFFFFF},
			{Name: "NVIDIA C", AdapterRAM: 0xFFEFFFFF},
		}, nil, []winAdapter{
			{Name: "NVIDIA A", Vendor: "nvidia"},
			{Name: "NVIDIA B", Vendor: "nvidia"},
			{Name: "NVIDIA C", Vendor: "nvidia", VRAMBytes: 0xFFEFFFFF},
		}},
		{"registry VRAM by exact name", []wmiVideoController{
			{Name: "NVIDIA GeForce RTX 3080", AdapterRAM: 0xFFF00000},
			{Name: "AMD Radeon RX 6600", AdapterRAM: 1 << 30},
			{Name: "Intel(R) UHD Graphics 770", AdapterRAM: 1 << 30},
			{Name: "Intel(R) Arc(TM) A380", AdapterRAM: 1 << 30},
		}, []winAdapter{
			{Name: "NVIDIA GeForce RTX 3080", VRAMBytes: 10 << 30},
			{Name: "AMD Radeon RX 6600", VRAMBytes: 8 << 30},
			{Name: "intel(r) uhd graphics 770", VRAMBytes: 2 << 30},
			{Name: "Intel(R) Arc(TM) A380"},
		}, []winAdapter{
			{Name: "NVIDIA GeForce RTX 3080", Vendor: "nvidia", VRAMBytes: 10 << 30},
			{Name: "AMD Radeon RX 6600", Vendor: "amd", VRAMBytes: 8 << 30},
			{Name: "Intel(R) UHD Graphics 770", Vendor: "intel", VRAMBytes: 1 << 30},
			{Name: "Intel(R) Arc(TM) A380", Vendor: "intel", VRAMBytes: 1 << 30},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := adaptersFromWMI(c.rows, c.registry); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestGPUsFromAdapters(t *testing.T) {
	gpus := gpusFromAdapters([]winAdapter{
		{Name: "NVIDIA GeForce RTX 3080", Vendor: "nvidia", VRAMBytes: 10 << 30},
		{Name: "AMD Radeon RX 6600"},
		{Name: "Video Controller", Vendor: "unknown"},
		{Name: "Mystery"},
	})
	if len(gpus) != 4 {
		t.Fatalf("gpus = %+v", gpus)
	}
	want := []struct{ name, vendor string }{
		{"NVIDIA GeForce RTX 3080", "nvidia"}, {"AMD Radeon RX 6600", "amd"}, {"Video Controller", "unknown"}, {"Mystery", "unknown"},
	}
	for i, g := range gpus {
		if g.Index != i || g.Name != want[i].name || g.Vendor != want[i].vendor {
			t.Fatalf("gpu %d = %+v", i, g)
		}
		if g.UtilPercent != nil || g.MemUsedBytes != nil || g.TempC != nil || g.PowerW != nil {
			t.Fatalf("gpu %d has live values: %+v", i, g)
		}
	}
	if gpus[0].MemTotalBytes == nil || *gpus[0].MemTotalBytes != 10<<30 {
		t.Fatalf("VRAM = %v", gpus[0].MemTotalBytes)
	}
	if gpus[1].MemTotalBytes != nil {
		t.Fatalf("zero VRAM must be absent, got %v", *gpus[1].MemTotalBytes)
	}
	if out := gpusFromAdapters(nil); out == nil || len(out) != 0 {
		t.Fatalf("got %#v", out)
	}
}

func TestErrWithFirstLine(t *testing.T) {
	base := errors.New("exit status 9")
	got := errWithFirstLine(base, []byte("\xef\xbb\xbf\r\nNVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver. \r\nMake sure...\r\n"))
	if !errors.Is(got, base) || got.Error() != "exit status 9: NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver." {
		t.Fatalf("got %v", got)
	}
	if got := errWithFirstLine(base, []byte(" \r\n")); got != base {
		t.Fatalf("blank output changed the error: %v", got)
	}
}

func TestProbeContextBoundedByTick(t *testing.T) {
	ctx, cancel := (Options{}).probeContext(time.Minute)
	if ctx.Err() != nil {
		t.Fatal("background probe context already done")
	}
	cancel()
	tick, stop := context.WithCancel(context.Background())
	stop()
	ctx, cancel = (Options{ctx: tick}).probeContext(time.Minute)
	defer cancel()
	if ctx.Err() == nil {
		t.Fatal("probe context outlived its tick")
	}
}

func TestGPUProbeSeesTickContext(t *testing.T) {
	type key struct{}
	var seen any
	g := newGPUCollector(testOpts(), func(o Options) []GPUSource { seen = o.ctx.Value(key{}); return nil })
	g.sample(context.WithValue(context.Background(), key{}, "tick"))
	if seen != "tick" {
		t.Fatalf("probe ctx value = %v", seen)
	}
}
