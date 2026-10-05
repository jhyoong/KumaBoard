package collectors

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeDRMCard lays out a sysfs-like tree: <root>/devices/<pci> is the PCI
// device with a driver symlink, and <root>/drm/cardN/device links to it.
// files are paths relative to cardN.
func fakeDRMCard(t *testing.T, root, card, driver, pci string, files map[string]string) {
	t.Helper()
	dev := filepath.Join(root, "devices", pci)
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../bus/pci/drivers/"+driver, filepath.Join(dev, "driver")); err != nil {
		t.Fatal(err)
	}
	cardDir := filepath.Join(root, "drm", card)
	if err := os.MkdirAll(cardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "devices", pci), filepath.Join(cardDir, "device")); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		writeSysfs(t, filepath.Join(cardDir, rel), content)
	}
}

func writeSysfs(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixtureDRM builds an amdgpu dGPU, an i915 iGPU, an NVIDIA card, a
// runtime-suspended amdgpu card, and entries that must be skipped.
func fixtureDRM(t *testing.T) string {
	root := t.TempDir()
	fakeDRMCard(t, root, "card0", "amdgpu", "0000:03:00.0", map[string]string{
		"device/vendor":                      "0x1002\n",
		"device/device":                      "0x73ff\n",
		"device/product_name":                "AMD Radeon RX 6600\n",
		"device/power/runtime_status":        "active\n",
		"device/gpu_busy_percent":            "37\n",
		"device/mem_info_vram_used":          "2254857830\n",
		"device/mem_info_vram_total":         "8573157376\n",
		"device/hwmon/hwmon3/temp1_input":    "61000\n",
		"device/hwmon/hwmon3/power1_average": "118000000\n",
	})
	fakeDRMCard(t, root, "card1", "i915", "0000:00:02.0", map[string]string{
		"device/vendor":               "0x8086\n",
		"device/device":               "0x1912\n",
		"device/power/runtime_status": "active\n",
		"gt/gt0/rc6_residency_ms":     "1000\n",
		"gt/gt0/rps_act_freq_mhz":     "350\n",
	})
	fakeDRMCard(t, root, "card2", "nvidia", "0000:01:00.0", map[string]string{
		"device/vendor":               "0x10de\n",
		"device/device":               "0x2206\n",
		"device/power/runtime_status": "active\n",
	})
	fakeDRMCard(t, root, "card3", "amdgpu", "0000:05:00.0", map[string]string{
		"device/vendor":               "0x1002\n",
		"device/device":               "0x7480\n",
		"device/power/runtime_status": "suspended\n",
		"device/gpu_busy_percent":     "99\n",
	})
	fakeDRMCard(t, root, "card4", "simple-framebuffer", "simple-framebuffer.0", nil)
	for _, skip := range []string{"card0-DP-1", "renderD128", "version"} {
		writeSysfs(t, filepath.Join(root, "drm", skip, "status"), "connected\n")
	}
	return filepath.Join(root, "drm")
}

func TestDRMProbe(t *testing.T) {
	s := newDRMSource(fixtureDRM(t), slog.New(slog.DiscardHandler))
	if len(s.cards) != 4 {
		t.Fatalf("cards = %d, want 4 (simpledrm, connectors, render nodes skipped)", len(s.cards))
	}
	want := []struct{ driver, vendor, name, bus string }{
		{"amdgpu", "amd", "AMD Radeon RX 6600", "0000:03:00.0"},
		{"i915", "intel", "Intel GPU 8086:1912", "0000:00:02.0"},
		{"nvidia", "nvidia", "NVIDIA GPU 10de:2206", "0000:01:00.0"},
		{"amdgpu", "amd", "AMD GPU 1002:7480", "0000:05:00.0"},
	}
	for i, w := range want {
		c := s.cards[i]
		if c.index != i || c.driver != w.driver || c.vendor != w.vendor || c.name != w.name || c.busID != w.bus {
			t.Fatalf("card %d = %+v, want %+v", i, c, w)
		}
	}
	if s.cards[1].resPath == "" {
		t.Fatal("i915 rc6 path not found")
	}
}

func TestDRMProbeNoGPU(t *testing.T) {
	if got := probeGPUs(Options{DRMRoot: t.TempDir(), Log: slog.New(slog.DiscardHandler)}); got != nil {
		t.Fatalf("got %v", got)
	}
	if got := probeGPUs(Options{DRMRoot: "/nonexistent/drm", Log: slog.New(slog.DiscardHandler)}); got != nil {
		t.Fatalf("got %v", got)
	}
}

func TestDRMSample(t *testing.T) {
	root := fixtureDRM(t)
	s := newDRMSource(root, slog.New(slog.DiscardHandler))
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	smiCalls := 0
	s.smiPath = "/usr/bin/nvidia-smi"
	s.run = func(context.Context, string) ([]byte, error) {
		smiCalls++
		return []byte(nvidiaSMIFixture), nil
	}

	gpus, err := s.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 4 {
		t.Fatalf("gpus = %+v", gpus)
	}
	amd, intel, nv, sleeping := gpus[0], gpus[1], gpus[2], gpus[3]

	if amd.Index != 0 || amd.Vendor != "amd" || *amd.UtilPercent != 37 || *amd.MemUsedBytes != 2254857830 ||
		*amd.MemTotalBytes != 8573157376 || *amd.TempC != 61 || *amd.PowerW != 118 {
		t.Fatalf("amdgpu = %+v", amd)
	}
	if intel.UtilPercent != nil || intel.MemTotalBytes != nil || intel.TempC != nil {
		t.Fatalf("i915 first tick must have no util/mem/temp: %+v", intel)
	}
	if nv.Name != "NVIDIA GeForce RTX 3080" || *nv.UtilPercent != 37 || *nv.MemTotalBytes != 10240<<20 || *nv.TempC != 61 || *nv.PowerW != 118.52 {
		t.Fatalf("nvidia = %+v", nv)
	}
	if !sleeping.Suspended || *sleeping.UtilPercent != 0 || sleeping.MemUsedBytes != nil || sleeping.TempC != nil {
		t.Fatalf("suspended card must report util 0 and nothing else: %+v", sleeping)
	}

	// 30 s later the iGPU spent 21 s in RC6: 30% busy.
	writeSysfs(t, filepath.Join(root, "card1", "gt", "gt0", "rc6_residency_ms"), "22000\n")
	now = now.Add(30 * time.Second)
	gpus, err = s.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u := gpus[1].UtilPercent; u == nil || *u < 29.99 || *u > 30.01 {
		t.Fatalf("i915 util = %v", u)
	}
	if smiCalls != 2 {
		t.Fatalf("nvidia-smi calls = %d", smiCalls)
	}
}

func TestDRMSuspendedNvidiaSkipsSpawn(t *testing.T) {
	root := t.TempDir()
	fakeDRMCard(t, root, "card0", "nvidia", "0000:01:00.0", map[string]string{
		"device/vendor":               "0x10de\n",
		"device/device":               "0x2206\n",
		"device/power/runtime_status": "suspended\n",
	})
	s := newDRMSource(filepath.Join(root, "drm"), slog.New(slog.DiscardHandler))
	s.smiPath = "/usr/bin/nvidia-smi"
	s.run = func(context.Context, string) ([]byte, error) {
		t.Fatal("nvidia-smi must not run while the card is runtime-suspended")
		return nil, nil
	}
	gpus, err := s.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 1 || !gpus[0].Suspended {
		t.Fatalf("got %+v", gpus)
	}
}

func TestDRMNvidiaSMIDisabledAfterFailures(t *testing.T) {
	s := newDRMSource(fixtureDRM(t), slog.New(slog.DiscardHandler))
	calls := 0
	s.smiPath = "/usr/bin/nvidia-smi"
	s.run = func(context.Context, string) ([]byte, error) {
		calls++
		return nil, errors.New("exit status 9")
	}
	for range 5 {
		gpus, err := s.Sample(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// The NVIDIA card is still listed, with name only.
		if gpus[2].Name != "NVIDIA GPU 10de:2206" || gpus[2].UtilPercent != nil {
			t.Fatalf("nvidia = %+v", gpus[2])
		}
	}
	if calls != gpuMaxFailures {
		t.Fatalf("nvidia-smi calls = %d, want %d", calls, gpuMaxFailures)
	}
}

func TestDRMCardsVanishIsAnError(t *testing.T) {
	root := fixtureDRM(t)
	s := newDRMSource(root, slog.New(slog.DiscardHandler))
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sample(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestCollectWithDRMFixture(t *testing.T) {
	m, err := New(Options{GPU: true, DRMRoot: fixtureDRM(t)}).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(m.GPUs) != 4 || m.GPUs[0].Name != "AMD Radeon RX 6600" {
		t.Fatalf("got %+v", m.GPUs)
	}
}
