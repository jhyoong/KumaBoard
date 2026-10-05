package collectors

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	defaultHwmonRoot   = "/sys/class/hwmon"
	defaultThermalRoot = "/sys/class/thermal"
)

// sysfsTempSource reads hwmon chips, falling back to thermal zones. Both are
// world-readable, so no privilege is needed. The tree is walked every tick;
// it is a handful of small sysfs reads.
type sysfsTempSource struct {
	hwmonRoot   string
	thermalRoot string
}

func probeTemp(opts Options) tempSource {
	s := &sysfsTempSource{hwmonRoot: opts.HwmonRoot, thermalRoot: opts.ThermalRoot}
	if s.hwmonRoot == "" {
		s.hwmonRoot = defaultHwmonRoot
	}
	if s.thermalRoot == "" {
		s.thermalRoot = defaultThermalRoot
	}
	if _, _, err := s.Sample(context.Background()); err != nil {
		return nil
	}
	return s
}

func (s *sysfsTempSource) Name() string { return "linux-sysfs" }

var errNoTemp = errors.New("no plausible CPU temperature")

func (s *sysfsTempSource) Sample(context.Context) (float64, string, error) {
	if v, label, ok := pickHwmon(readHwmonChips(s.hwmonRoot)); ok {
		return v, label, nil
	}
	if v, label, ok := pickThermalZone(readThermalZones(s.thermalRoot)); ok {
		return v, label, nil
	}
	return 0, "", errNoTemp
}

// readHwmonChips lists <root>/hwmon*, skipping chips whose device is a DRM
// GPU (owned by the GPU collector) whatever their name.
func readHwmonChips(root string) []hwmonChip {
	dirs, _ := filepath.Glob(filepath.Join(root, "hwmon*"))
	sort.Strings(dirs)
	var chips []hwmonChip
	for _, d := range dirs {
		name := readString(filepath.Join(d, "name"))
		if name == "" || hwmonExcluded(name) || isDRMDevice(filepath.Join(d, "device")) {
			continue
		}
		inputs, _ := filepath.Glob(filepath.Join(d, "temp*_input"))
		sort.Strings(inputs)
		c := hwmonChip{Name: name}
		for _, in := range inputs {
			base := strings.TrimSuffix(filepath.Base(in), "_input")
			v, err := readMilliC(in)
			if err != nil {
				// -EIO/-ENODATA from a sensor without a reading.
				continue
			}
			label := readString(filepath.Join(d, base+"_label"))
			if label == "" {
				label = base
			}
			c.Inputs = append(c.Inputs, hwmonInput{Label: label, C: v})
		}
		chips = append(chips, c)
	}
	return chips
}

// isDRMDevice reports whether a hwmon device link points at a device with a
// drm/ class directory, i.e. a GPU.
func isDRMDevice(dev string) bool {
	p, err := filepath.EvalSymlinks(dev)
	if err != nil {
		return false
	}
	fi, err := os.Stat(filepath.Join(p, "drm"))
	return err == nil && fi.IsDir()
}

func readThermalZones(root string) []thermalZone {
	dirs, _ := filepath.Glob(filepath.Join(root, "thermal_zone*"))
	sort.Strings(dirs)
	zones := make([]thermalZone, 0, len(dirs))
	for _, d := range dirs {
		z := thermalZone{Dir: filepath.Base(d), Type: readString(filepath.Join(d, "type"))}
		if v, err := readMilliC(filepath.Join(d, "temp")); err == nil {
			z.C, z.OK = v, true
		}
		zones = append(zones, z)
	}
	return zones
}

// readMilliC reads a sysfs millidegree Celsius value (may be negative).
func readMilliC(path string) (float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, err
	}
	return float64(v) / 1000, nil
}
