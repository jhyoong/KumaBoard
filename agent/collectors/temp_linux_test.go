package collectors

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeHwmon adds <root>/<dir> with a name and files relative to it.
func fakeHwmon(t *testing.T, root, dir, name string, files map[string]string) {
	t.Helper()
	writeSysfs(t, filepath.Join(root, dir, "name"), name+"\n")
	for f, v := range files {
		writeSysfs(t, filepath.Join(root, dir, f), v)
	}
}

// fixtureHwmon is an AMD desktop: k10temp, an NVMe drive that runs hotter, an
// amdgpu card, a GPU hwmon under an unexpected name whose device is a DRM
// card, and an ACPI zone.
func fixtureHwmon(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	fakeHwmon(t, root, "hwmon0", "acpitz", map[string]string{"temp1_input": "27800\n"})
	fakeHwmon(t, root, "hwmon1", "nvme", map[string]string{"temp1_input": "70850\n", "temp1_label": "Composite\n"})
	fakeHwmon(t, root, "hwmon2", "k10temp", map[string]string{
		"temp1_input": "54000\n", "temp1_label": "Tctl\n",
		"temp3_input": "61250\n", "temp3_label": "Tccd1\n",
	})
	fakeHwmon(t, root, "hwmon3", "amdgpu", map[string]string{"temp1_input": "65000\n", "temp1_label": "edge\n"})
	// A GPU hwmon with a CPU-looking name; its device has a drm/ directory.
	dev := filepath.Join(root, "devices", "0000:03:00.0")
	if err := os.MkdirAll(filepath.Join(dev, "drm", "card1"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeHwmon(t, root, "hwmon4", "zenpower", map[string]string{"temp1_input": "99000\n"})
	if err := os.Symlink(dev, filepath.Join(root, "hwmon4", "device")); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestReadHwmonChips(t *testing.T) {
	chips := readHwmonChips(fixtureHwmon(t))
	if len(chips) != 2 || chips[0].Name != "acpitz" || chips[1].Name != "k10temp" {
		t.Fatalf("chips = %+v (nvme, amdgpu and DRM-owned chips must be skipped)", chips)
	}
	k := chips[1]
	if len(k.Inputs) != 2 || k.Inputs[0] != (hwmonInput{Label: "Tctl", C: 54}) || k.Inputs[1] != (hwmonInput{Label: "Tccd1", C: 61.25}) {
		t.Fatalf("k10temp inputs = %+v", k.Inputs)
	}
	if chips[0].Inputs[0].Label != "temp1" {
		t.Fatalf("unlabelled input = %+v", chips[0].Inputs)
	}
}

func TestSysfsTempHwmon(t *testing.T) {
	src := probeTemp(Options{HwmonRoot: fixtureHwmon(t), ThermalRoot: t.TempDir()})
	if src == nil {
		t.Fatal("probe found nothing")
	}
	v, label, err := src.Sample(context.Background())
	if err != nil || v != 54 || label != "k10temp/Tctl" {
		t.Fatalf("got %v %q %v", v, label, err)
	}
}

func TestSysfsTempCoretemp(t *testing.T) {
	root := t.TempDir()
	fakeHwmon(t, root, "hwmon0", "coretemp", map[string]string{
		"temp1_input": "48000\n", "temp1_label": "Package id 0\n",
		"temp2_input": "51000\n", "temp2_label": "Core 0\n",
	})
	fakeHwmon(t, root, "hwmon1", "iwlwifi_1", map[string]string{"temp1_input": "60000\n"})
	src := probeTemp(Options{HwmonRoot: root, ThermalRoot: t.TempDir()})
	if src == nil {
		t.Fatal("probe found nothing")
	}
	if v, label, err := src.Sample(context.Background()); err != nil || v != 48 || label != "coretemp/Package id 0" {
		t.Fatalf("got %v %q %v", v, label, err)
	}
}

// TestSysfsTempThermalZoneFallback is a Pi kernel whose cpu-thermal zone has
// no hwmon.
func TestSysfsTempThermalZoneFallback(t *testing.T) {
	hw := t.TempDir()
	fakeHwmon(t, hw, "hwmon0", "rpi_volt", nil)
	th := t.TempDir()
	writeSysfs(t, filepath.Join(th, "thermal_zone0", "type"), "cpu-thermal\n")
	writeSysfs(t, filepath.Join(th, "thermal_zone0", "temp"), "47236\n")
	writeSysfs(t, filepath.Join(th, "cooling_device0", "type"), "gpio-fan\n")
	src := probeTemp(Options{HwmonRoot: hw, ThermalRoot: th})
	if src == nil {
		t.Fatal("probe found nothing")
	}
	if v, label, err := src.Sample(context.Background()); err != nil || v != 47.236 || label != "cpu-thermal/thermal_zone0" {
		t.Fatalf("got %v %q %v", v, label, err)
	}
}

func TestSysfsTempNoSensor(t *testing.T) {
	hw := t.TempDir()
	// A sensor with a bogus zero reading is "no reading", not 0°C.
	fakeHwmon(t, hw, "hwmon0", "cpu_thermal", map[string]string{"temp1_input": "0\n"})
	fakeHwmon(t, hw, "hwmon1", "nvme", map[string]string{"temp1_input": "40000\n"})
	if src := probeTemp(Options{HwmonRoot: hw, ThermalRoot: t.TempDir()}); src != nil {
		t.Fatalf("probe = %v, want nil", src)
	}
	if src := probeTemp(Options{HwmonRoot: filepath.Join(hw, "missing"), ThermalRoot: filepath.Join(hw, "missing")}); src != nil {
		t.Fatalf("probe = %v, want nil", src)
	}
}

func TestCollectWithHwmonFixture(t *testing.T) {
	m, err := New(Options{HwmonRoot: fixtureHwmon(t), ThermalRoot: t.TempDir()}).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.TempC == nil || *m.TempC != 54 || m.TempSensor != "k10temp/Tctl" {
		t.Fatalf("temp = %v %q", m.TempC, m.TempSensor)
	}
}

func TestCollectWithoutSensorOmitsTemp(t *testing.T) {
	m, err := New(Options{HwmonRoot: t.TempDir(), ThermalRoot: t.TempDir()}).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.TempC != nil || m.TempSensor != "" {
		t.Fatalf("temp = %v %q, want absent", m.TempC, m.TempSensor)
	}
}
