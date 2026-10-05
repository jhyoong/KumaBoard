package collectors

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/jhyoong/KumaBoard/proto"
)

var (
	modPDH                           = windows.NewLazySystemDLL("pdh.dll")
	procPdhOpenQueryW                = modPDH.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounterW        = modPDH.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData          = modPDH.NewProc("PdhCollectQueryData")
	procPdhGetFormattedCounterArrayW = modPDH.NewProc("PdhGetFormattedCounterArrayW")
	procPdhCloseQuery                = modPDH.NewProc("PdhCloseQuery")
)

const (
	pdhFmtDouble        = 0x00000200
	pdhFmtNoCap100      = 0x00008000
	pdhMoreData         = 0x800007D2
	pdhCstatusValidData = 0x0
	pdhCstatusNewData   = 0x1

	counterEngine    = `\GPU Engine(*)\Utilization Percentage`
	counterDedicated = `\GPU Adapter Memory(*)\Dedicated Usage`
	counterShared    = `\GPU Adapter Memory(*)\Shared Usage`

	displayClassKey = `SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}`
)

// pdhFmtCounterValueItem mirrors PDH_FMT_COUNTERVALUE_ITEM_W on 64-bit
// Windows with PDH_FMT_DOUBLE: name pointer, CStatus, padding, double.
type pdhFmtCounterValueItem struct {
	name    *uint16
	cstatus uint32
	value   float64
}

// pdhSource keeps one PDH query open for the life of the process, because
// GPU Engine utilisation is a rate counter that needs the previous collection.
type pdhSource struct {
	query     uintptr
	engine    uintptr
	dedicated uintptr
	shared    uintptr
	adapters  []winAdapter
	smiPath   string
	smi       failGate
	log       *slog.Logger
}

// gpuProbeTimeout caps each fallback probe step (a first nvidia-smi run or a
// WMI query can be slow right after boot). The probing tick's context bounds
// the whole probe.
const gpuProbeTimeout = 4 * time.Second

// probeGPUs tries, in order: PDH counters (every vendor, Task Manager's
// numbers), nvidia-smi on its own, then adapter enumeration (name and vendor
// only). Every failed step is reported once, so "none detected" is never the
// only line in the log.
func probeGPUs(opts Options) []GPUSource {
	log := opts.Log
	var attempts []probeAttempt
	src := func() GPUSource {
		pdh, err := newPDHSource(log)
		if err == nil {
			return pdh
		}
		attempts = append(attempts, probeAttempt{"pdh GPU Engine/GPU Adapter Memory counters", err})

		smi, err := newSMISource(opts)
		if err == nil {
			return smi
		}
		attempts = append(attempts, probeAttempt{"nvidia-smi", err})

		ad, err := newAdapterSource(opts)
		if err == nil {
			return ad
		}
		attempts = append(attempts, probeAttempt{"adapter enumeration", err})
		return nil
	}()
	if src == nil {
		log.Warn("gpu: every probe failed", "attempts", attemptStrings(attempts))
		return nil
	}
	if len(attempts) > 0 {
		log.Warn("gpu: using fallback backend", "backend", src.Name(), "failed", attemptStrings(attempts))
	}
	return []GPUSource{src}
}

// smiSource samples NVIDIA GPUs through nvidia-smi alone, when PDH is
// unavailable to the service account.
type smiSource struct{ path string }

func newSMISource(opts Options) (*smiSource, error) {
	path, tried := findNvidiaSMI()
	if path == "" {
		return nil, fmt.Errorf("nvidia-smi.exe not found (tried %s)", strings.Join(tried, "; "))
	}
	s := &smiSource{path: path}
	ctx, cancel := opts.probeContext(gpuProbeTimeout)
	defer cancel()
	if _, err := s.Sample(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *smiSource) Name() string { return "windows-nvidia-smi" }

func (s *smiSource) Sample(ctx context.Context) ([]proto.GPU, error) {
	raw, err := runNvidiaSMI(ctx, s.path)
	if err != nil {
		if d := execErrDetail(err); d != err {
			err = d
		} else {
			err = errWithFirstLine(err, raw)
		}
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	rows, err := parseNvidiaSMI(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	return gpusFromNvidiaRows(rows), nil
}

// adapterSource reports the adapters found at probe time by name and vendor,
// with no utilisation. It is the last resort.
type adapterSource struct{ gpus []proto.GPU }

func newAdapterSource(opts Options) (*adapterSource, error) {
	reg := readDisplayAdapters()
	ctx, cancel := opts.probeContext(gpuProbeTimeout)
	defer cancel()
	var rows []wmiVideoController
	err := wmiQuery(ctx, `SELECT Name, AdapterCompatibility, AdapterRAM, PNPDeviceID FROM Win32_VideoController`, &rows, wmiNamespaceCIMv2)
	adapters := adaptersFromWMI(rows, reg)
	if len(adapters) == 0 {
		adapters = reg
	}
	if len(adapters) == 0 {
		if err != nil {
			return nil, fmt.Errorf("Win32_VideoController: %w; display-class registry key: no hardware adapters", err)
		}
		return nil, errors.New("Win32_VideoController and display-class registry key list no hardware adapters")
	}
	return &adapterSource{gpus: gpusFromAdapters(adapters)}, nil
}

func (s *adapterSource) Name() string { return "windows-adapters" }

func (s *adapterSource) Sample(context.Context) ([]proto.GPU, error) {
	return append([]proto.GPU(nil), s.gpus...), nil
}

func newPDHSource(log *slog.Logger) (*pdhSource, error) {
	if err := modPDH.Load(); err != nil {
		return nil, err
	}
	s := &pdhSource{log: log}
	if r, _, _ := procPdhOpenQueryW.Call(0, 0, uintptr(unsafe.Pointer(&s.query))); r != 0 {
		return nil, pdhError("PdhOpenQuery", r)
	}
	for _, c := range []struct {
		path string
		h    *uintptr
	}{{counterEngine, &s.engine}, {counterDedicated, &s.dedicated}, {counterShared, &s.shared}} {
		p, err := windows.UTF16PtrFromString(c.path)
		if err != nil {
			s.close()
			return nil, err
		}
		if r, _, _ := procPdhAddEnglishCounterW.Call(s.query, uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(c.h))); r != 0 {
			s.close()
			return nil, pdhError("PdhAddEnglishCounter "+c.path, r)
		}
	}
	// Prime the rate counter; utilisation is available from the next collect.
	if r, _, _ := procPdhCollectQueryData.Call(s.query); r != 0 {
		s.close()
		return nil, pdhError("PdhCollectQueryData", r)
	}
	engines, _ := s.values(s.engine, pdhFmtDouble|pdhFmtNoCap100)
	ded, _ := s.values(s.dedicated, pdhFmtDouble)
	shr, _ := s.values(s.shared, pdhFmtDouble)
	if len(engines) == 0 && len(ded) == 0 && len(shr) == 0 {
		s.close()
		return nil, errors.New("no GPU counter instances")
	}
	s.adapters = readDisplayAdapters()
	for _, a := range s.adapters {
		if vendorFromName(a.Name) == "nvidia" {
			s.smiPath, _ = findNvidiaSMI()
			break
		}
	}
	return s, nil
}

func (s *pdhSource) Name() string { return "windows-pdh" }

func (s *pdhSource) Sample(ctx context.Context) ([]proto.GPU, error) {
	if r, _, _ := procPdhCollectQueryData.Call(s.query); r != 0 {
		return nil, pdhError("PdhCollectQueryData", r)
	}
	engines, errE := s.values(s.engine, pdhFmtDouble|pdhFmtNoCap100)
	ded, errD := s.values(s.dedicated, pdhFmtDouble)
	shr, errS := s.values(s.shared, pdhFmtDouble)
	if errE != nil && errD != nil && errS != nil {
		return nil, errE
	}
	gpus := buildWindowsGPUs(engines, ded, shr, s.adapters)
	s.fillNvidia(ctx, gpus)
	return gpus, nil
}

// fillNvidia adds temperature and power from nvidia-smi when exactly one
// NVIDIA adapter was identified and nvidia-smi lists exactly one GPU.
func (s *pdhSource) fillNvidia(ctx context.Context, gpus []proto.GPU) {
	if s.smiPath == "" || s.smi.disabled {
		return
	}
	var target *proto.GPU
	for i := range gpus {
		if gpus[i].Vendor == "nvidia" {
			if target != nil {
				return
			}
			target = &gpus[i]
		}
	}
	if target == nil {
		return
	}
	raw, err := runNvidiaSMI(ctx, s.smiPath)
	var rows []nvidiaRow
	if err == nil {
		rows, err = parseNvidiaSMI(raw)
	}
	s.smi.record(err, s.log, "nvidia-smi")
	if err != nil || len(rows) != 1 {
		return
	}
	target.TempC, target.PowerW = rows[0].TempC, rows[0].PowerW
}

// values returns the valid instances of a wildcard counter by instance name.
func (s *pdhSource) values(counter uintptr, format uint32) (map[string]float64, error) {
	// The instance set can grow between the size query and the fetch.
	for range 3 {
		var size, count uint32
		r, _, _ := procPdhGetFormattedCounterArrayW.Call(counter, uintptr(format),
			uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), 0)
		if r == 0 {
			return map[string]float64{}, nil
		}
		if r != pdhMoreData {
			return nil, pdhError("PdhGetFormattedCounterArray", r)
		}
		buf := make([]byte, size)
		r, _, _ = procPdhGetFormattedCounterArrayW.Call(counter, uintptr(format),
			uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&buf[0])))
		if r == pdhMoreData {
			continue
		}
		if r != 0 {
			return nil, pdhError("PdhGetFormattedCounterArray", r)
		}
		out := make(map[string]float64, count)
		if count > 0 {
			items := unsafe.Slice((*pdhFmtCounterValueItem)(unsafe.Pointer(&buf[0])), count)
			for _, it := range items {
				if it.cstatus != pdhCstatusValidData && it.cstatus != pdhCstatusNewData {
					continue
				}
				out[windows.UTF16PtrToString(it.name)] += it.value
			}
		}
		runtime.KeepAlive(buf)
		return out, nil
	}
	return nil, errors.New("PdhGetFormattedCounterArray: instance set kept growing")
}

func (s *pdhSource) close() {
	procPdhCloseQuery.Call(s.query)
}

func pdhError(op string, r uintptr) error {
	return fmt.Errorf("%s: PDH status 0x%08X", op, uint32(r))
}

// readDisplayAdapters lists hardware adapters from the display-class key,
// which is readable by Users. Software adapters are skipped.
func readDisplayAdapters() []winAdapter {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, displayClassKey, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil
	}
	defer k.Close()
	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return nil
	}
	var out []winAdapter
	for _, n := range names {
		// Adapter instances are 0000, 0001, ...; skip "Properties" and friends.
		if _, err := strconv.Atoi(n); err != nil || len(n) != 4 {
			continue
		}
		sk, err := registry.OpenKey(k, n, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		desc, _, err := sk.GetStringValue("DriverDesc")
		if err == nil && desc != "" && !isSoftwareAdapter(desc) {
			out = append(out, winAdapter{Name: desc, VRAMBytes: readVRAM(sk)})
		}
		sk.Close()
	}
	return out
}

// readVRAM reads the adapter's memory size. Drivers store it as a QWORD or a
// 4/8-byte binary value, under qwMemorySize (newer) or MemorySize (older).
func readVRAM(k registry.Key) uint64 {
	for _, name := range []string{"HardwareInformation.qwMemorySize", "HardwareInformation.MemorySize"} {
		if v, _, err := k.GetIntegerValue(name); err == nil && v > 0 {
			return v
		}
		if b, _, err := k.GetBinaryValue(name); err == nil {
			switch len(b) {
			case 8:
				return binary.LittleEndian.Uint64(b)
			case 4:
				return uint64(binary.LittleEndian.Uint32(b))
			}
		}
	}
	return 0
}

// findNvidiaSMI resolves nvidia-smi.exe once, to an absolute path. tried
// lists the locations checked.
func findNvidiaSMI() (path string, tried []string) {
	path, tried = resolveNvidiaSMI(smiResolver{
		lookPath: exec.LookPath,
		exists: func(p string) bool {
			fi, err := os.Stat(p)
			return err == nil && !fi.IsDir()
		},
		glob: func(pattern string) []string {
			m, _ := filepath.Glob(pattern)
			return m
		},
		getenv: os.Getenv,
	})
	if path != "" {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
	}
	return path, tried
}
