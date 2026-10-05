package upgrade

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSwapCreatesOld(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "kuma-agent")
	if runtime.GOOS == "windows" {
		current = filepath.Join(dir, "kuma-agent.exe")
	}
	os.WriteFile(current, []byte("old-binary"), 0o755)
	temp := filepath.Join(dir, "kuma-agent.new")
	os.WriteFile(temp, []byte("new-binary"), 0o755)

	if err := Swap(current, temp); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(current)
	if string(got) != "new-binary" {
		t.Fatalf("current = %q", got)
	}
	old, _ := os.ReadFile(current + ".old")
	if string(old) != "old-binary" {
		t.Fatalf(".old = %q", old)
	}
}

func TestSwapOverwritesExistingOld(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "kuma-agent")
	if runtime.GOOS == "windows" {
		current = filepath.Join(dir, "kuma-agent.exe")
	}
	os.WriteFile(current, []byte("v2"), 0o755)
	os.WriteFile(current+".old", []byte("v1"), 0o755)
	temp := filepath.Join(dir, "kuma-agent.new")
	os.WriteFile(temp, []byte("v3"), 0o755)

	if err := Swap(current, temp); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(current)
	if string(got) != "v3" {
		t.Fatalf("current = %q", got)
	}
	old, _ := os.ReadFile(current + ".old")
	if string(old) != "v2" {
		t.Fatalf(".old = %q", old)
	}
}

func TestRestoreFromOld(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "kuma-agent")
	if runtime.GOOS == "windows" {
		current = filepath.Join(dir, "kuma-agent.exe")
	}
	os.WriteFile(current, []byte("bad"), 0o755)
	os.WriteFile(current+".old", []byte("good"), 0o755)

	if err := RestoreOld(current); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(current)
	if string(got) != "good" {
		t.Fatalf("current = %q", got)
	}
}
