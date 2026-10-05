package collectors

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/jhyoong/KumaBoard/proto"
)

// IOKit and CoreFoundation are called through purego (no cgo), following
// gopsutil's internal/common/common_darwin.go. Library handles are never
// closed; see gopsutil issue 1832.
const (
	iokitPath          = "/System/Library/Frameworks/IOKit.framework/IOKit"
	coreFoundationPath = "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation"

	kIOMainPortDefault   = 0
	cfStringEncodingUTF8 = 0x08000100
	cfNumberSInt64Type   = 4
)

// darwinIOKit holds the resolved functions. Arguments that point at Go
// memory are unsafe.Pointer, slices, or Go pointers, never uintptr; CF and
// IOKit object references are plain handles and stay uintptr/uint32.
type darwinIOKit struct {
	IOServiceMatching               func(name string) uintptr
	IOServiceGetMatchingServices    func(mainPort uint32, matching uintptr, existing *uint32) int32
	IOIteratorNext                  func(iterator uint32) uint32
	IOObjectRelease                 func(object uint32) int32
	IORegistryEntryCreateCFProperty func(entry uint32, key, allocator uintptr, options uint32) uintptr

	CFStringCreateWithCString func(alloc uintptr, cStr string, encoding uint32) uintptr
	CFDictionaryGetValue      func(dict, key uintptr) uintptr
	CFGetTypeID               func(cf uintptr) uint
	CFNumberGetTypeID         func() uint
	CFStringGetTypeID         func() uint
	CFDataGetTypeID           func() uint
	CFDictionaryGetTypeID     func() uint
	CFNumberGetValue          func(num uintptr, theType int64, valuePtr unsafe.Pointer) bool
	CFStringGetCString        func(s uintptr, buf []byte, size int64, encoding uint32) bool
	CFDataGetLength           func(data uintptr) int64
	CFDataGetBytePtr          func(data uintptr) unsafe.Pointer
	CFRelease                 func(cf uintptr)
}

// appleSource reads IOAccelerator → PerformanceStatistics. Unprivileged.
type appleSource struct {
	k        *darwinIOKit
	keyPerf  uintptr
	keyModel uintptr
	statKeys map[string]uintptr
	numberID uint
	stringID uint
	dataID   uint
	dictID   uint
}

func probeGPUs(opts Options) []GPUSource {
	s, err := newAppleSource()
	if err != nil {
		opts.Log.Debug("gpu: iokit probe", "err", err)
		return nil
	}
	if gpus, err := s.Sample(context.Background()); err != nil || len(gpus) == 0 {
		return nil
	}
	return []GPUSource{s}
}

// newAppleSource resolves symbols. purego panics on a missing symbol; the
// caller's recover turns that into "no GPU".
func newAppleSource() (*appleSource, error) {
	iokit, err := purego.Dlopen(iokitPath, purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, err
	}
	cf, err := purego.Dlopen(coreFoundationPath, purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		return nil, err
	}
	k := &darwinIOKit{}
	purego.RegisterLibFunc(&k.IOServiceMatching, iokit, "IOServiceMatching")
	purego.RegisterLibFunc(&k.IOServiceGetMatchingServices, iokit, "IOServiceGetMatchingServices")
	purego.RegisterLibFunc(&k.IOIteratorNext, iokit, "IOIteratorNext")
	purego.RegisterLibFunc(&k.IOObjectRelease, iokit, "IOObjectRelease")
	purego.RegisterLibFunc(&k.IORegistryEntryCreateCFProperty, iokit, "IORegistryEntryCreateCFProperty")
	purego.RegisterLibFunc(&k.CFStringCreateWithCString, cf, "CFStringCreateWithCString")
	purego.RegisterLibFunc(&k.CFDictionaryGetValue, cf, "CFDictionaryGetValue")
	purego.RegisterLibFunc(&k.CFGetTypeID, cf, "CFGetTypeID")
	purego.RegisterLibFunc(&k.CFNumberGetTypeID, cf, "CFNumberGetTypeID")
	purego.RegisterLibFunc(&k.CFStringGetTypeID, cf, "CFStringGetTypeID")
	purego.RegisterLibFunc(&k.CFDataGetTypeID, cf, "CFDataGetTypeID")
	purego.RegisterLibFunc(&k.CFDictionaryGetTypeID, cf, "CFDictionaryGetTypeID")
	purego.RegisterLibFunc(&k.CFNumberGetValue, cf, "CFNumberGetValue")
	purego.RegisterLibFunc(&k.CFStringGetCString, cf, "CFStringGetCString")
	purego.RegisterLibFunc(&k.CFDataGetLength, cf, "CFDataGetLength")
	purego.RegisterLibFunc(&k.CFDataGetBytePtr, cf, "CFDataGetBytePtr")
	purego.RegisterLibFunc(&k.CFRelease, cf, "CFRelease")

	// Keys live for the process; they are never released.
	s := &appleSource{
		k:        k,
		keyPerf:  k.CFStringCreateWithCString(0, "PerformanceStatistics", cfStringEncodingUTF8),
		keyModel: k.CFStringCreateWithCString(0, "model", cfStringEncodingUTF8),
		statKeys: map[string]uintptr{},
		numberID: k.CFNumberGetTypeID(),
		stringID: k.CFStringGetTypeID(),
		dataID:   k.CFDataGetTypeID(),
		dictID:   k.CFDictionaryGetTypeID(),
	}
	if s.keyPerf == 0 || s.keyModel == 0 {
		return nil, errors.New("CFStringCreateWithCString failed")
	}
	for _, name := range applePerfStatKeys {
		key := k.CFStringCreateWithCString(0, name, cfStringEncodingUTF8)
		if key == 0 {
			return nil, errors.New("CFStringCreateWithCString failed")
		}
		s.statKeys[name] = key
	}
	return s, nil
}

func (s *appleSource) Name() string { return "darwin-iokit" }

// Sample re-enumerates IOAccelerator services each tick; it is a handful of
// mach calls and needs no state.
func (s *appleSource) Sample(context.Context) ([]proto.GPU, error) {
	matching := s.k.IOServiceMatching("IOAccelerator")
	if matching == 0 {
		return nil, errors.New("IOServiceMatching failed")
	}
	var iter uint32
	// Consumes the matching dictionary.
	if kr := s.k.IOServiceGetMatchingServices(kIOMainPortDefault, matching, &iter); kr != 0 {
		return nil, fmt.Errorf("IOServiceGetMatchingServices: 0x%x", uint32(kr))
	}
	defer s.k.IOObjectRelease(iter)
	var out []proto.GPU
	for svc := s.k.IOIteratorNext(iter); svc != 0; svc = s.k.IOIteratorNext(iter) {
		g, ok := s.read(svc, len(out))
		s.k.IOObjectRelease(svc)
		if ok {
			out = append(out, g)
		}
	}
	return out, nil
}

func (s *appleSource) read(svc uint32, index int) (proto.GPU, bool) {
	perf := s.k.IORegistryEntryCreateCFProperty(svc, s.keyPerf, 0, 0)
	if perf == 0 {
		return proto.GPU{}, false
	}
	defer s.k.CFRelease(perf)
	if s.k.CFGetTypeID(perf) != s.dictID {
		return proto.GPU{}, false
	}
	stats := map[string]int64{}
	for name, key := range s.statKeys {
		// Get, not Copy: the value is owned by perf and must not be released.
		v := s.k.CFDictionaryGetValue(perf, key)
		if v == 0 || s.k.CFGetTypeID(v) != s.numberID {
			continue
		}
		var n int64
		if s.k.CFNumberGetValue(v, cfNumberSInt64Type, unsafe.Pointer(&n)) {
			stats[name] = n
		}
	}
	return appleGPU(index, s.model(svc), stats), true
}

// model reads the accelerator's "model" property ("Apple M2 Max"), which is
// a CFString on current macOS and CFData on some older releases.
func (s *appleSource) model(svc uint32) string {
	v := s.k.IORegistryEntryCreateCFProperty(svc, s.keyModel, 0, 0)
	if v == 0 {
		return ""
	}
	defer s.k.CFRelease(v)
	switch s.k.CFGetTypeID(v) {
	case s.stringID:
		buf := make([]byte, 256)
		if s.k.CFStringGetCString(v, buf, int64(len(buf)), cfStringEncodingUTF8) {
			return cString(buf)
		}
	case s.dataID:
		n := s.k.CFDataGetLength(v)
		if p := s.k.CFDataGetBytePtr(v); p != nil && n > 0 {
			return cString(unsafe.Slice((*byte)(p), min(n, 256)))
		}
	}
	return ""
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}
