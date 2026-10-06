package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhyoong/KumaBoard/agent/upgrade"
)

// A running agent classifies a failed upgrade selftest as "the target
// version rejected this device's config" by finding this text in the child's
// stderr, and the child may be any released version. It must never change.
func TestSelftestPrefixForBadConfig(t *testing.T) {
	const prefix = "selftest: config:"
	if upgrade.SelftestConfigStage != prefix {
		t.Fatalf("upgrade.SelftestConfigStage = %q, want %q", upgrade.SelftestConfigStage, prefix)
	}

	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, path := range map[string]string{
		"missing file":    filepath.Join(dir, "absent.yaml"),
		"malformed yaml":  write("malformed.yaml", "device: [unclosed\n"),
		"invalid setting": write("invalid.yaml", "device:\n  name: Not_A_Valid_Name\n"),
	} {
		err := runSelftest([]string{"-config", path})
		if err == nil {
			t.Errorf("%s: selftest passed", name)
			continue
		}
		// main prints "error: " + err, which is what the parent reads.
		if !strings.HasPrefix(err.Error(), prefix+" ") {
			t.Errorf("%s: error %q does not start with %q", name, err, prefix)
		}
	}
}
