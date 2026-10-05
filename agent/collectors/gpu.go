package collectors

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
)

const (
	// gpuMaxFailures consecutive errors disable a backend (or the nvidia-smi
	// helper) for the life of the process.
	gpuMaxFailures = 3
	// gpuTimeout caps one GPU sample, including any nvidia-smi spawn.
	gpuTimeout = 5 * time.Second
)

// GPUSource is one GPU backend. Sample is called once per metrics tick, never
// concurrently.
type GPUSource interface {
	Name() string
	Sample(ctx context.Context) ([]proto.GPU, error)
}

type gpuBackend struct {
	src      GPUSource
	fails    int
	disabled bool
}

// gpuCollector probes once, then samples every live backend. An empty probe
// result disables GPU sampling for the life of the process.
type gpuCollector struct {
	opts     Options
	probe    func(Options) []GPUSource
	probed   bool
	backends []*gpuBackend
}

func newGPUCollector(opts Options, probe func(Options) []GPUSource) *gpuCollector {
	return &gpuCollector{opts: opts, probe: probe}
}

func (g *gpuCollector) sample(ctx context.Context) []proto.GPU {
	log := g.opts.Log
	if !g.probed {
		g.probed = true
		g.opts.ctx = ctx
		srcs := g.safeProbe()
		if len(srcs) == 0 {
			log.Info("gpu: none detected")
			return nil
		}
		names := make([]string, 0, len(srcs))
		for _, s := range srcs {
			g.backends = append(g.backends, &gpuBackend{src: s})
			names = append(names, s.Name())
		}
		log.Info("gpu: detected", "backends", names)
	}
	if len(g.backends) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, gpuTimeout)
	defer cancel()
	var out []proto.GPU
	for _, b := range g.backends {
		if b.disabled {
			continue
		}
		gpus, err := safeSample(ctx, b.src)
		if err != nil {
			b.fails++
			if b.fails >= gpuMaxFailures {
				b.disabled = true
				log.Warn("gpu: backend disabled after repeated failures", "backend", b.src.Name(), "err", err)
			}
			continue
		}
		b.fails = 0
		out = append(out, gpus...)
	}
	return out
}

// safeProbe keeps a panicking platform probe from taking the agent down.
func (g *gpuCollector) safeProbe() (srcs []GPUSource) {
	defer func() {
		if r := recover(); r != nil {
			g.opts.Log.Warn("gpu: probe panicked", "panic", r)
			srcs = nil
		}
	}()
	return g.probe(g.opts)
}

func safeSample(ctx context.Context, s GPUSource) (gpus []proto.GPU, err error) {
	defer func() {
		if r := recover(); r != nil {
			gpus, err = nil, fmt.Errorf("panic: %v", r)
		}
	}()
	return s.Sample(ctx)
}

// failGate disables an optional helper after gpuMaxFailures consecutive errors.
type failGate struct {
	fails    int
	disabled bool
}

func (f *failGate) record(err error, log *slog.Logger, what string) {
	if err == nil {
		f.fails = 0
		return
	}
	f.fails++
	if f.fails >= gpuMaxFailures && !f.disabled {
		f.disabled = true
		log.Warn("gpu: helper disabled after repeated failures", "helper", what, "err", err)
	}
}

func ptr[T any](v T) *T { return &v }

// vendorFromPCI maps a PCI vendor ID ("0x1002") to a vendor name.
func vendorFromPCI(id string) string {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "0x1002":
		return "amd"
	case "0x8086":
		return "intel"
	case "0x10de":
		return "nvidia"
	case "0x106b":
		return "apple"
	}
	return "unknown"
}

// vendorFromName guesses the vendor from a marketing name.
func vendorFromName(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasPrefix(n, "apple"):
		return "apple"
	case strings.Contains(n, "nvidia"), strings.Contains(n, "geforce"), strings.Contains(n, "quadro"):
		return "nvidia"
	case strings.Contains(n, "amd"), strings.Contains(n, "radeon"):
		return "amd"
	case strings.Contains(n, "intel"):
		return "intel"
	}
	return "unknown"
}

// residencyBusy converts an idle-residency counter delta (RC6 or gtidle, in
// ms) over a wall-clock interval into a busy percentage. ok is false for the
// first sample, a counter reset, or a zero interval.
func residencyBusy(prevMs, curMs uint64, wall time.Duration) (float64, bool) {
	if curMs < prevMs || wall <= 0 {
		return 0, false
	}
	idle := float64(curMs - prevMs)
	w := float64(wall) / float64(time.Millisecond)
	return min(max(100*(1-idle/w), 0), 100), true
}

// ---- nvidia-smi (Linux and Windows) ----

// nvidiaSMIArgs requests one CSV line per GPU. pci.bus_id is last so rows can
// be matched to DRM cards on Linux.
var nvidiaSMIArgs = []string{
	"--query-gpu=index,name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw,pci.bus_id",
	"--format=csv,noheader,nounits",
}

type nvidiaRow struct {
	Index    int
	Name     string
	Util     *float64
	MemUsed  *uint64 // bytes
	MemTotal *uint64 // bytes
	TempC    *float64
	PowerW   *float64
	BusID    string // normalised, e.g. "0000:01:00.0"
}

func runNvidiaSMI(ctx context.Context, path string) ([]byte, error) {
	return exec.CommandContext(ctx, path, nvidiaSMIArgs...).Output()
}

// parseNvidiaSMI parses the output of nvidiaSMIArgs. Fields the driver cannot
// report ("[N/A]", "[Not Supported]") are left nil.
func parseNvidiaSMI(out []byte) ([]nvidiaRow, error) {
	r := csv.NewReader(bytes.NewReader(cleanSMIOutput(out)))
	r.TrimLeadingSpace = true
	r.FieldsPerRecord = len(strings.Split(strings.TrimPrefix(nvidiaSMIArgs[0], "--query-gpu="), ","))
	recs, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	if len(recs) == 0 {
		return nil, fmt.Errorf("nvidia-smi: no GPUs listed")
	}
	rows := make([]nvidiaRow, 0, len(recs))
	for _, f := range recs {
		idx, err := strconv.Atoi(strings.TrimSpace(f[0]))
		if err != nil {
			return nil, fmt.Errorf("nvidia-smi: bad index %q", f[0])
		}
		row := nvidiaRow{Index: idx, Name: strings.TrimSpace(f[1]), BusID: normalizeBusID(f[7])}
		row.Util = smiFloat(f[2])
		if v := smiFloat(f[3]); v != nil {
			row.MemUsed = ptr(uint64(*v) << 20)
		}
		if v := smiFloat(f[4]); v != nil {
			row.MemTotal = ptr(uint64(*v) << 20)
		}
		row.TempC = smiFloat(f[5])
		row.PowerW = smiFloat(f[6])
		rows = append(rows, row)
	}
	return rows, nil
}

// cleanSMIOutput strips what a Windows console tool may wrap its output in: a
// UTF-8 byte-order mark, NUL padding and bare carriage returns.
func cleanSMIOutput(out []byte) []byte {
	out = bytes.TrimPrefix(out, []byte("\xef\xbb\xbf"))
	out = bytes.ReplaceAll(out, []byte{0}, nil)
	out = bytes.ReplaceAll(out, []byte("\r\n"), []byte("\n"))
	out = bytes.ReplaceAll(out, []byte("\r"), []byte("\n"))
	return bytes.TrimLeft(out, "\n")
}

func smiFloat(s string) *float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 {
		return nil
	}
	return &v
}

// normalizeBusID turns nvidia-smi's "00000000:01:00.0" into sysfs's
// "0000:01:00.0".
func normalizeBusID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	dom, rest, ok := strings.Cut(s, ":")
	if !ok {
		return s
	}
	d, err := strconv.ParseUint(dom, 16, 32)
	if err != nil {
		return s
	}
	return fmt.Sprintf("%04x:%s", d, rest)
}

func applyNvidiaRow(g *proto.GPU, r nvidiaRow) {
	if r.Name != "" {
		g.Name = r.Name
	}
	g.UtilPercent, g.MemUsedBytes, g.MemTotalBytes = r.Util, r.MemUsed, r.MemTotal
	g.TempC, g.PowerW = r.TempC, r.PowerW
}

// gpusFromNvidiaRows builds samples straight from nvidia-smi, for hosts where
// it is the only working backend.
func gpusFromNvidiaRows(rows []nvidiaRow) []proto.GPU {
	out := make([]proto.GPU, 0, len(rows))
	for _, r := range rows {
		g := proto.GPU{Index: r.Index, Name: "NVIDIA GPU", Vendor: "nvidia"}
		applyNvidiaRow(&g, r)
		out = append(out, g)
	}
	return out
}

// execErrDetail appends the first stderr line of a failed command to its
// error, so "exit status 9" says why.
func execErrDetail(err error) error {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return err
	}
	return errWithFirstLine(err, ee.Stderr)
}

// errWithFirstLine appends the first non-blank line of a command's output to
// its error. nvidia-smi prints its failure reason on stdout.
func errWithFirstLine(err error, out []byte) error {
	line, _, _ := strings.Cut(strings.TrimSpace(string(cleanSMIOutput(out))), "\n")
	if line == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, strings.TrimSpace(line))
}

// probeAttempt records one detection source that was tried and why it failed,
// for the single startup log line.
type probeAttempt struct {
	Probe string
	Err   error
}

func attemptStrings(as []probeAttempt) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Probe+": "+a.Err.Error())
	}
	return out
}

// ---- Windows nvidia-smi resolution (pure) ----

// nvidiaSMICandidates lists the absolute paths nvidia-smi.exe is installed
// at, most likely first: System32 (current drivers), the NVSMI directory
// (older drivers), and the driver store (DCH drivers; a glob pattern). A
// service does not inherit the interactive user's PATH, so PATH alone is not
// enough. Paths are built with backslashes whatever the host OS.
func nvidiaSMICandidates(getenv func(string) string) []string {
	const exe = "nvidia-smi.exe"
	root := getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	out := []string{root + `\System32\` + exe}
	seen := map[string]bool{}
	for _, v := range []string{"ProgramFiles", "ProgramW6432", "ProgramFiles(x86)"} {
		dir := strings.TrimRight(getenv(v), `\`)
		if dir == "" || seen[strings.ToLower(dir)] {
			continue
		}
		seen[strings.ToLower(dir)] = true
		out = append(out, dir+`\NVIDIA Corporation\NVSMI\`+exe)
	}
	if !seen[`c:\program files`] {
		out = append(out, `C:\Program Files\NVIDIA Corporation\NVSMI\`+exe)
	}
	return append(out, root+`\System32\DriverStore\FileRepository\nv*\`+exe)
}

// smiResolver holds the filesystem lookups resolveNvidiaSMI needs. Tests
// inject fakes.
type smiResolver struct {
	lookPath func(string) (string, error)
	exists   func(string) bool
	glob     func(string) []string
	getenv   func(string) string
}

// resolveNvidiaSMI finds nvidia-smi.exe: PATH first, then every candidate.
// tried lists each location checked, for the not-found log line.
func resolveNvidiaSMI(r smiResolver) (path string, tried []string) {
	tried = append(tried, "PATH")
	if p, err := r.lookPath("nvidia-smi.exe"); err == nil && p != "" {
		return p, tried
	}
	for _, c := range nvidiaSMICandidates(r.getenv) {
		tried = append(tried, c)
		if strings.Contains(c, "*") {
			if m := r.glob(c); len(m) > 0 {
				sort.Strings(m)
				// The newest driver package sorts last often enough; any works.
				return m[len(m)-1], tried
			}
			continue
		}
		if r.exists(c) {
			return c, tried
		}
	}
	return "", tried
}

// ---- Windows adapter enumeration (pure) ----

// wmiVideoController is the subset of Win32_VideoController that is read.
// AdapterRAM is a uint32 and saturates just under 4 GiB.
type wmiVideoController struct {
	Name                 string
	AdapterCompatibility string
	AdapterRAM           uint32
	PNPDeviceID          string
}

// adaptersFromWMI converts Win32_VideoController rows, dropping software
// adapters. A saturated AdapterRAM is discarded; the registry value, matched
// by name, replaces it when present.
func adaptersFromWMI(rows []wmiVideoController, registry []winAdapter) []winAdapter {
	var out []winAdapter
	for _, r := range rows {
		name := strings.TrimSpace(r.Name)
		if name == "" || isSoftwareAdapter(name) {
			continue
		}
		a := winAdapter{Name: name, Vendor: vendorFromName(name)}
		if a.Vendor == "unknown" {
			a.Vendor = vendorFromName(r.AdapterCompatibility)
			if strings.Contains(strings.ToLower(r.AdapterCompatibility), "advanced micro devices") {
				a.Vendor = "amd"
			}
		}
		if a.Vendor == "unknown" {
			if i := strings.Index(strings.ToUpper(r.PNPDeviceID), "VEN_"); i >= 0 && len(r.PNPDeviceID) >= i+8 {
				a.Vendor = vendorFromPCI("0x" + r.PNPDeviceID[i+4:i+8])
			}
		}
		if r.AdapterRAM > 0 && r.AdapterRAM < 0xFFF00000 {
			a.VRAMBytes = uint64(r.AdapterRAM)
		}
		for _, reg := range registry {
			if reg.Name == name && reg.VRAMBytes > 0 {
				a.VRAMBytes = reg.VRAMBytes
			}
		}
		out = append(out, a)
	}
	return out
}

// gpusFromAdapters reports adapters by name and vendor only, for hosts where
// no utilisation source works.
func gpusFromAdapters(adapters []winAdapter) []proto.GPU {
	out := make([]proto.GPU, 0, len(adapters))
	for i, a := range adapters {
		g := proto.GPU{Index: i, Name: a.Name, Vendor: a.Vendor}
		if g.Vendor == "" {
			g.Vendor = vendorFromName(a.Name)
		}
		if a.VRAMBytes > 0 {
			g.MemTotalBytes = ptr(a.VRAMBytes)
		}
		out = append(out, g)
	}
	return out
}

// ---- Windows PDH shapes (pure, so they are tested on every OS) ----

// pdhEngineInstance splits a "\GPU Engine(*)" instance name such as
// "pid_1234_luid_0x00000000_0x0000C9E1_phys_0_eng_0_engtype_3D" into the
// adapter LUID and engine type.
func pdhEngineInstance(name string) (luid, engtype string, ok bool) {
	luid, ok = pdhLUID(name)
	if !ok {
		return "", "", false
	}
	_, engtype, ok = strings.Cut(name, "_engtype_")
	if !ok || engtype == "" {
		return "", "", false
	}
	return luid, engtype, true
}

// pdhLUID extracts "0x00000000_0x0000C9E1" from any GPU counter instance
// name containing "luid_0x…_0x…".
func pdhLUID(name string) (string, bool) {
	_, rest, ok := strings.Cut(name, "luid_")
	if !ok {
		return "", false
	}
	parts := strings.SplitN(rest, "_", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "0x") || !strings.HasPrefix(parts[1], "0x") {
		return "", false
	}
	return strings.ToLower(parts[0] + "_" + parts[1]), true
}

// aggregateEngineUtil matches Task Manager: per adapter, sum every process's
// utilisation per engine type, clamp each sum to 100, then take the maximum
// over engine types.
func aggregateEngineUtil(engines map[string]float64) map[string]float64 {
	perType := map[[2]string]float64{}
	for inst, v := range engines {
		luid, engtype, ok := pdhEngineInstance(inst)
		if !ok || v < 0 {
			continue
		}
		perType[[2]string{luid, engtype}] += v
	}
	out := map[string]float64{}
	for k, v := range perType {
		out[k[0]] = max(out[k[0]], min(v, 100))
	}
	return out
}

// winAdapter is a hardware display adapter from the display-class registry key
// or Win32_VideoController.
type winAdapter struct {
	Name      string
	Vendor    string // empty when only the name is known
	VRAMBytes uint64
}

// buildWindowsGPUs assembles per-adapter samples from PDH counter arrays keyed
// by instance name. An adapter that has neither dedicated nor shared memory in
// use is inactive or software-only and is skipped. The registry cannot be
// mapped to a LUID directly, so the name and VRAM total are attached only when
// the registry lists exactly one hardware adapter and exactly one LUID has
// dedicated memory in use; otherwise the LUID is the name.
func buildWindowsGPUs(engines, dedicated, shared map[string]float64, adapters []winAdapter) []proto.GPU {
	util := aggregateEngineUtil(engines)
	sumByLUID := func(m map[string]float64) map[string]float64 {
		out := map[string]float64{}
		for inst, v := range m {
			if luid, ok := pdhLUID(inst); ok && v > 0 {
				out[luid] += v
			}
		}
		return out
	}
	ded, shr := sumByLUID(dedicated), sumByLUID(shared)
	var luids, dedLUIDs []string
	seen := map[string]bool{}
	for _, m := range []map[string]float64{util, ded, shr} {
		for l := range m {
			if seen[l] || (ded[l] == 0 && shr[l] == 0) {
				continue
			}
			seen[l] = true
			luids = append(luids, l)
			if ded[l] > 0 {
				dedLUIDs = append(dedLUIDs, l)
			}
		}
	}
	sort.Strings(luids)
	out := make([]proto.GPU, 0, len(luids))
	for i, l := range luids {
		g := proto.GPU{Index: i, Name: "GPU " + l, Vendor: "unknown"}
		if u, ok := util[l]; ok {
			g.UtilPercent = ptr(u)
		}
		// A discrete adapter's working set is its dedicated memory; an iGPU
		// has none and works out of shared system memory.
		if d := ded[l]; d > 0 {
			g.MemUsedBytes = ptr(uint64(d))
		} else {
			g.MemUsedBytes = ptr(uint64(shr[l]))
		}
		if len(adapters) == 1 && len(dedLUIDs) == 1 && dedLUIDs[0] == l {
			g.Name = adapters[0].Name
			g.Vendor = vendorFromName(adapters[0].Name)
			if adapters[0].VRAMBytes > 0 {
				g.MemTotalBytes = ptr(adapters[0].VRAMBytes)
			}
		}
		out = append(out, g)
	}
	return out
}

// isSoftwareAdapter filters Basic Render/Display, Remote Display, Hyper-V.
func isSoftwareAdapter(desc string) bool {
	return strings.HasPrefix(desc, "Microsoft ")
}

// ---- Apple IOAccelerator PerformanceStatistics (pure) ----

// applePerfStatKeys are the PerformanceStatistics entries read on darwin.
var applePerfStatKeys = []string{"Device Utilization %", "GPU Activity(%)", "In use system memory"}

// appleGPU builds a sample from an IOAccelerator's model string and its
// PerformanceStatistics numbers. Unified memory has no VRAM total, so
// MemTotalBytes stays absent.
func appleGPU(index int, model string, stats map[string]int64) proto.GPU {
	g := proto.GPU{Index: index, Name: model, Vendor: vendorFromName(model)}
	if g.Name == "" {
		g.Name, g.Vendor = "Apple GPU", "apple"
	}
	if v, ok := stats["Device Utilization %"]; ok {
		g.UtilPercent = ptr(float64(v))
	} else if v, ok := stats["GPU Activity(%)"]; ok {
		g.UtilPercent = ptr(float64(v))
	}
	if v, ok := stats["In use system memory"]; ok && v >= 0 {
		g.MemUsedBytes = ptr(uint64(v))
	}
	return g
}
