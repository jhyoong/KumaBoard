package collectors

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
)

const defaultDRMRoot = "/sys/class/drm"

var vendorLabel = map[string]string{"amd": "AMD", "intel": "Intel", "nvidia": "NVIDIA", "apple": "Apple", "unknown": "Unknown"}

// cardRe matches primary nodes and skips connectors such as card0-DP-1.
var cardRe = regexp.MustCompile(`^card([0-9]+)$`)

// drmCard is one GPU found under /sys/class/drm at probe time.
type drmCard struct {
	index  int
	dir    string // <root>/cardN
	dev    string // <root>/cardN/device
	driver string
	vendor string
	name   string
	busID  string
	// resPath is the Intel idle-residency counter (RC6 or gtidle), in ms.
	resPath  string
	prevRes  uint64
	prevAt   time.Time
	havePrev bool
}

// drmSource reads amdgpu, i915, and xe cards from sysfs, and NVIDIA cards
// through nvidia-smi when it is installed.
type drmSource struct {
	cards   []*drmCard
	smiPath string
	smi     failGate
	run     func(ctx context.Context, path string) ([]byte, error)
	now     func() time.Time
	log     *slog.Logger
}

func probeGPUs(opts Options) []GPUSource {
	root := opts.DRMRoot
	if root == "" {
		root = defaultDRMRoot
	}
	s := newDRMSource(root, opts.Log)
	if len(s.cards) == 0 {
		return nil
	}
	return []GPUSource{s}
}

func newDRMSource(root string, log *slog.Logger) *drmSource {
	s := &drmSource{run: runNvidiaSMI, now: time.Now, log: log}
	entries, err := os.ReadDir(root)
	if err != nil {
		return s
	}
	hasNvidia := false
	for _, e := range entries {
		m := cardRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		idx, _ := strconv.Atoi(m[1])
		c := &drmCard{index: idx, dir: filepath.Join(root, e.Name())}
		c.dev = filepath.Join(c.dir, "device")
		target, err := os.Readlink(filepath.Join(c.dev, "driver"))
		if err != nil {
			continue
		}
		c.driver = filepath.Base(target)
		switch c.driver {
		case "amdgpu", "i915", "xe", "nvidia":
		default:
			// simpledrm, vc4/v3d (Pi), virtio, ...: nothing useful to report.
			continue
		}
		c.vendor = vendorFromPCI(readString(filepath.Join(c.dev, "vendor")))
		c.name = readString(filepath.Join(c.dev, "product_name"))
		if c.name == "" {
			c.name = fmt.Sprintf("%s GPU %s:%s", vendorLabel[c.vendor],
				strings.TrimPrefix(readString(filepath.Join(c.dev, "vendor")), "0x"),
				strings.TrimPrefix(readString(filepath.Join(c.dev, "device")), "0x"))
		}
		if p, err := filepath.EvalSymlinks(c.dev); err == nil {
			c.busID = strings.ToLower(filepath.Base(p))
		}
		switch c.driver {
		case "i915":
			c.resPath = firstExisting(filepath.Join(c.dir, "gt", "gt0", "rc6_residency_ms"),
				filepath.Join(c.dir, "power", "rc6_residency_ms"))
		case "xe":
			c.resPath = firstExisting(filepath.Join(c.dev, "tile0", "gt0", "gtidle", "idle_residency_ms"))
		case "nvidia":
			hasNvidia = true
		}
		s.cards = append(s.cards, c)
	}
	sort.Slice(s.cards, func(i, j int) bool { return s.cards[i].index < s.cards[j].index })
	if hasNvidia {
		if p, err := exec.LookPath("nvidia-smi"); err == nil {
			if abs, err := filepath.Abs(p); err == nil {
				s.smiPath = abs
			}
		}
	}
	return s
}

func (s *drmSource) Name() string { return "linux-drm" }

func (s *drmSource) Sample(ctx context.Context) ([]proto.GPU, error) {
	out := make([]proto.GPU, 0, len(s.cards))
	var nv []int // positions in out of awake NVIDIA cards
	gone := 0
	for _, c := range s.cards {
		if _, err := os.Stat(c.dir); err != nil {
			gone++
			continue
		}
		g := proto.GPU{Index: c.index, Name: c.name, Vendor: c.vendor}
		// Never touch sensors (or spawn nvidia-smi) on a runtime-suspended
		// card: that would wake it.
		if readString(filepath.Join(c.dev, "power", "runtime_status")) == "suspended" {
			markSuspended(&g, c)
			out = append(out, g)
			continue
		}
		switch c.driver {
		case "amdgpu":
			s.readAMD(c, &g)
		case "i915", "xe":
			s.readIntel(c, &g)
		case "nvidia":
			nv = append(nv, len(out))
		}
		out = append(out, g)
	}
	if gone == len(s.cards) {
		return nil, errors.New("drm: no cards readable")
	}
	if len(nv) > 0 {
		s.fillNvidia(ctx, out, nv)
	}
	return out, nil
}

func markSuspended(g *proto.GPU, c *drmCard) {
	g.Suspended = true
	g.UtilPercent = ptr(0.0)
	c.havePrev = false
}

func (s *drmSource) readAMD(c *drmCard, g *proto.GPU) {
	busy, err := readUint(filepath.Join(c.dev, "gpu_busy_percent"))
	if errors.Is(err, syscall.EBUSY) {
		// Suspended between the status check and the read.
		markSuspended(g, c)
		return
	}
	if err == nil {
		g.UtilPercent = ptr(float64(busy))
	}
	if v, err := readUint(filepath.Join(c.dev, "mem_info_vram_used")); err == nil {
		g.MemUsedBytes = ptr(v)
	}
	if v, err := readUint(filepath.Join(c.dev, "mem_info_vram_total")); err == nil {
		g.MemTotalBytes = ptr(v)
	}
	readHwmon(c.dev, g)
}

// readIntel derives busy% from the idle-residency delta since the last tick;
// the first tick after start or resume has no utilisation.
func (s *drmSource) readIntel(c *drmCard, g *proto.GPU) {
	if c.resPath != "" {
		v, err := readUint(c.resPath)
		now := s.now()
		if err != nil {
			c.havePrev = false
		} else {
			if c.havePrev {
				if u, ok := residencyBusy(c.prevRes, v, now.Sub(c.prevAt)); ok {
					g.UtilPercent = ptr(u)
				}
			}
			c.prevRes, c.prevAt, c.havePrev = v, now, true
		}
	}
	readHwmon(c.dev, g)
}

func (s *drmSource) fillNvidia(ctx context.Context, out []proto.GPU, nv []int) {
	if s.smiPath == "" || s.smi.disabled {
		return
	}
	raw, err := s.run(ctx, s.smiPath)
	var rows []nvidiaRow
	if err == nil {
		rows, err = parseNvidiaSMI(raw)
	}
	s.smi.record(err, s.log, "nvidia-smi")
	if err != nil {
		return
	}
	byBus := map[string]nvidiaRow{}
	for _, r := range rows {
		byBus[r.BusID] = r
	}
	for i, pos := range nv {
		card := s.cardAt(out[pos].Index)
		if r, ok := byBus[card.busID]; ok && card.busID != "" {
			applyNvidiaRow(&out[pos], r)
		} else if len(rows) == len(nv) {
			applyNvidiaRow(&out[pos], rows[i])
		}
	}
}

func (s *drmSource) cardAt(index int) *drmCard {
	for _, c := range s.cards {
		if c.index == index {
			return c
		}
	}
	return &drmCard{}
}

// readHwmon adds temperature (temp1_input, m°C) and power (power1_average or
// power1_input, µW) from the device's first hwmon, if it has one.
func readHwmon(dev string, g *proto.GPU) {
	dirs, _ := filepath.Glob(filepath.Join(dev, "hwmon", "hwmon*"))
	if len(dirs) == 0 {
		return
	}
	sort.Strings(dirs)
	h := dirs[0]
	if v, err := readUint(filepath.Join(h, "temp1_input")); err == nil {
		g.TempC = ptr(float64(v) / 1000)
	}
	for _, f := range []string{"power1_average", "power1_input"} {
		if v, err := readUint(filepath.Join(h, f)); err == nil {
			g.PowerW = ptr(float64(v) / 1e6)
			break
		}
	}
}

func readString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

func firstExisting(paths ...string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
