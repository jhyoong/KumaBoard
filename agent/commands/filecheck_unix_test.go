//go:build !windows

package commands

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func killZero(pid int) error { return syscall.Kill(pid, 0) }

// checkTree is a directory tree for permission tests. A temp dir always sits
// under directories the test user owns, so the checker is told to trust
// everything down to the tree's root and, unless a test says otherwise, to
// act as some other non-root account. The access(2) half of the check then
// runs for real against the modes the test sets.
type checkTree struct {
	t    *testing.T
	root string
	c    *fileChecker
}

func newCheckTree(t *testing.T) *checkTree {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root passes every access check; the root rule is covered by TestRootRule")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other := os.Geteuid() + 1
	return &checkTree{t: t, root: root, c: &fileChecker{trusted: root, euid: &other}}
}

// dir creates a directory under the root and returns its path. It is made
// read-only by lock, and writable again at cleanup so the temp dir can go.
func (ct *checkTree) dir(rel string) string {
	ct.t.Helper()
	p := filepath.Join(ct.root, rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		ct.t.Fatal(err)
	}
	ct.t.Cleanup(func() { os.Chmod(p, 0o755) })
	return p
}

func (ct *checkTree) file(dir, name string, mode os.FileMode) string {
	ct.t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o644); err != nil {
		ct.t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		ct.t.Fatal(err)
	}
	return p
}

func (ct *checkTree) lock(dirs ...string) {
	ct.t.Helper()
	for _, d := range dirs {
		if err := os.Chmod(d, 0o555); err != nil {
			ct.t.Fatal(err)
		}
	}
}

func (ct *checkTree) wantOK(path, resolved string) {
	ct.t.Helper()
	got, err := ct.c.resolve(path)
	if err != nil {
		ct.t.Fatalf("%s: unexpected failure: %v", path, err)
	}
	if got != resolved {
		ct.t.Fatalf("%s resolved to %s, want %s", path, got, resolved)
	}
}

func (ct *checkTree) wantFail(path, contains string) {
	ct.t.Helper()
	got, err := ct.c.resolve(path)
	if err == nil {
		ct.t.Fatalf("%s: passed (resolved %s), want failure containing %q", path, got, contains)
	}
	if !strings.Contains(err.Error(), contains) {
		ct.t.Fatalf("%s: error %q does not contain %q", path, err, contains)
	}
}

func TestCheckFileSystemBinary(t *testing.T) {
	// The real check, no knobs: a system shell is not ours to modify.
	got, err := CheckFile("/bin/sh")
	if err != nil {
		t.Skipf("/bin/sh does not pass on this machine: %v", err)
	}
	fi, err := os.Lstat(got)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("resolved %q is not a regular file: %v", got, err)
	}
}

func TestCheckFileRejectsOwnTempDir(t *testing.T) {
	// The real check, no knobs: anything the test account created fails.
	p := filepath.Join(t.TempDir(), "s.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o555); err != nil {
		t.Fatal(err)
	}
	if got, err := CheckFile(p); err == nil {
		t.Fatalf("a script in the test's own temp dir passed: %s", got)
	}
}

func TestCheckFileModes(t *testing.T) {
	ct := newCheckTree(t)
	d := ct.dir("scripts")
	ro := ct.file(d, "ro.sh", 0o555)
	rw := ct.file(d, "rw.sh", 0o755)
	ct.lock(d)

	ct.wantOK(ro, ro)
	ct.wantFail(rw, rw+" is writable by the agent account")

	// The directory holding a read-only script must be read-only too.
	if err := os.Chmod(d, 0o755); err != nil {
		t.Fatal(err)
	}
	ct.wantFail(ro, d+" is writable by the agent account")
}

func TestCheckFileOwner(t *testing.T) {
	ct := newCheckTree(t)
	d := ct.dir("scripts")
	ro := ct.file(d, "ro.sh", 0o555)
	ct.lock(d)
	ct.wantOK(ro, ro)
	// Same modes, but now the agent account is the owner: it could chmod
	// its way back in, and under a read-only mount this is the only half
	// of the check that still fires.
	me := os.Geteuid()
	ct.c.euid = &me
	ct.wantFail(ro, " is owned by the agent account")
}

func TestCheckFileEveryDirectoryOnThePath(t *testing.T) {
	ct := newCheckTree(t)
	outer := ct.dir("outer")
	inner := ct.dir("outer/inner")
	f := ct.file(inner, "s.sh", 0o555)
	ct.lock(inner)
	// outer is still writable: inner could be renamed away and replaced.
	ct.wantFail(f, outer+" is writable by the agent account")
	ct.lock(outer)
	ct.wantOK(f, f)
}

func TestCheckFileSymlinks(t *testing.T) {
	ct := newCheckTree(t)
	safe := ct.dir("safe")
	other := ct.dir("other")
	loose := ct.dir("loose")
	target := ct.file(safe, "real.sh", 0o555)
	for link, to := range map[string]string{
		filepath.Join(safe, "abs.sh"):   target,
		filepath.Join(safe, "rel.sh"):   "real.sh",
		filepath.Join(other, "up.sh"):   "../safe/./real.sh",
		filepath.Join(other, "dir"):     safe,
		filepath.Join(other, "hop.sh"):  "dir/rel.sh",
		filepath.Join(loose, "link.sh"): target,
		filepath.Join(safe, "out.sh"):   filepath.Join(loose, "w.sh"),
		filepath.Join(safe, "loop-a"):   "loop-b",
		filepath.Join(safe, "loop-b"):   "loop-a",
		filepath.Join(safe, "dangling"): "nowhere",
	} {
		if err := os.Symlink(to, link); err != nil {
			t.Fatal(err)
		}
	}
	ct.file(loose, "w.sh", 0o555)
	ct.lock(safe, other)

	// Links in protected directories to a protected file resolve to it.
	ct.wantOK(filepath.Join(safe, "abs.sh"), target)
	ct.wantOK(filepath.Join(safe, "rel.sh"), target)
	ct.wantOK(filepath.Join(other, "up.sh"), target)
	ct.wantOK(filepath.Join(other, "hop.sh"), target)
	ct.wantOK(filepath.Join(other, "dir", "real.sh"), target)

	// A link that sits in a writable directory can be repointed.
	ct.wantFail(filepath.Join(loose, "link.sh"), loose+" is writable by the agent account")
	// A protected link that leads into a writable directory.
	ct.wantFail(filepath.Join(safe, "out.sh"), loose+" is writable by the agent account")

	ct.wantFail(filepath.Join(safe, "loop-a"), "too many symbolic links")
	ct.wantFail(filepath.Join(safe, "dangling"), "does not exist")
}

func TestCheckFileShape(t *testing.T) {
	ct := newCheckTree(t)
	d := ct.dir("scripts")
	f := ct.file(d, "s.sh", 0o555)
	ct.lock(d)
	ct.wantFail("scripts/s.sh", "not an absolute path")
	ct.wantFail(d, "is not a regular file")
	ct.wantFail(filepath.Join(d, "missing.sh"), "does not exist")
	ct.wantFail(filepath.Join(f, "below"), "not a directory")
	ct.wantOK(filepath.Join(d, ".", "..", "scripts", "s.sh"), f)
}

// As root every access probe succeeds, so the rule is mode bits alone.
func TestRootRule(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	zero := 0
	c := &fileChecker{trusted: root, euid: &zero}
	d := filepath.Join(root, "scripts")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(d, "s.sh")
	if err := os.WriteFile(f, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := c.resolve(f); err != nil || got != f {
		t.Fatalf("0755 dir, 0644 file: %q %v", got, err)
	}
	for _, mode := range []os.FileMode{0o664, 0o646} {
		os.Chmod(f, mode)
		if _, err := c.resolve(f); err == nil || !strings.Contains(err.Error(), f+" is group- or world-writable") {
			t.Fatalf("file %o: %v", mode, err)
		}
	}
	os.Chmod(f, 0o755)
	for _, mode := range []os.FileMode{0o775, 0o757, 0o1777} {
		os.Chmod(d, mode)
		if _, err := c.resolve(f); err == nil || !strings.Contains(err.Error(), d+" is group- or world-writable") {
			t.Fatalf("dir %o: %v", mode, err)
		}
	}
}
