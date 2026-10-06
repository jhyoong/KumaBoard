package commands

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jhyoong/KumaBoard/agent/config"
	"github.com/jhyoong/KumaBoard/proto"
)

// maxSymlinks bounds symlink resolution, like the kernel's ELOOP limit.
const maxSymlinks = 40

// CheckFile verifies that the agent's own account cannot modify what path
// will execute, and returns the fully resolved path to run. The path is
// walked component by component with symlinks followed by hand; the final
// file and every directory on the way (so also each directory holding a
// symlink) must not be modifiable by this process. A compromised agent, or a
// web-terminal session running as the same account, then cannot swap the
// content behind a dashboard button.
func CheckFile(path string) (string, error) {
	return (&fileChecker{}).resolve(path)
}

// fileChecker carries the knobs tests need; the zero value is the real check.
type fileChecker struct {
	// trusted, if set, is a directory whose ancestors (and itself) are not
	// checked. Tests only: a temp dir always sits under directories the test
	// user owns.
	trusted string
	// euid overrides the effective uid on Unix when non-nil. Tests only.
	euid *int
}

func (c *fileChecker) resolve(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s is not an absolute path", path)
	}
	root, rest, err := splitRoot(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	cur := root
	if err := c.checkEntry(cur); err != nil {
		return "", err
	}
	links := 0
	for len(rest) > 0 {
		name := rest[0]
		rest = rest[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			// The parent was traversed, and so checked, on the way here.
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, name)
		fi, err := os.Lstat(next)
		if err != nil {
			return "", describe(next, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			if links++; links > maxSymlinks {
				return "", fmt.Errorf("%s: too many symbolic links", path)
			}
			target, err := os.Readlink(next)
			if err != nil {
				return "", describe(next, err)
			}
			if target == "" {
				return "", fmt.Errorf("%s is an empty symbolic link", next)
			}
			// The link itself needs no check: replacing it takes write
			// access to cur, which was just ruled out.
			if !filepath.IsAbs(target) && os.IsPathSeparator(target[0]) {
				target = filepath.VolumeName(cur) + target // Windows: rooted, no drive
			}
			if filepath.IsAbs(target) {
				var trest []string
				if root, trest, err = splitRoot(target); err != nil {
					return "", err
				}
				cur = root
				if err := c.checkEntry(cur); err != nil {
					return "", err
				}
				rest = append(trest, rest...)
			} else {
				rest = append(splitComponents(target), rest...)
			}
			continue
		}
		if fi.Mode()&os.ModeIrregular != 0 {
			// Windows junctions and other reparse points.
			return "", fmt.Errorf("%s is a reparse point, which is not supported", next)
		}
		if err := c.check(next, fi); err != nil {
			return "", err
		}
		cur = next
	}
	fi, err := os.Lstat(cur)
	if err != nil {
		return "", describe(cur, err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", cur)
	}
	return cur, nil
}

// checkEntry stats and checks one already-resolved path (a volume root).
func (c *fileChecker) checkEntry(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return describe(p, err)
	}
	return c.check(p, fi)
}

func (c *fileChecker) check(p string, fi os.FileInfo) error {
	if c.trusted != "" && fi.IsDir() && (p == c.trusted || strings.HasPrefix(c.trusted, strings.TrimSuffix(p, string(filepath.Separator))+string(filepath.Separator))) {
		return nil
	}
	return c.notWritable(p, fi)
}

// splitRoot splits a cleaned absolute path into its root and the components
// below it.
func splitRoot(p string) (root string, rest []string, err error) {
	vol := filepath.VolumeName(p)
	if strings.HasPrefix(vol, `\\`) {
		return "", nil, fmt.Errorf("%s is on a network share, which cannot be checked", p)
	}
	root = vol + string(filepath.Separator)
	return root, splitComponents(strings.TrimPrefix(p, vol)), nil
}

func splitComponents(p string) []string {
	return strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == filepath.Separator })
}

// describe turns a stat error into a reason fit for the dashboard.
func describe(p string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%s does not exist", p)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("%s cannot be read by the agent account", p)
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %v", p, pe.Err)
	}
	return fmt.Errorf("%s: %v", p, err)
}

// checkCommand applies check to everything cmd would execute that it names by
// absolute path: its script, and run[0]. It returns the argv to run, with the
// script replaced by its resolved path, and the resolved program to execute
// (empty when run[0] is a bare name looked up in the fixed PATH).
func checkCommand(cmd config.Command, check func(string) (string, error)) (argv []string, program string, err error) {
	argv = cmd.Argv()
	if cmd.Script != "" {
		resolved, err := check(cmd.Script)
		if err != nil {
			return nil, "", err
		}
		argv[len(argv)-1] = resolved
		if len(cmd.Run) == 0 {
			return argv, resolved, nil
		}
	}
	if filepath.IsAbs(argv[0]) {
		if program, err = check(argv[0]); err != nil {
			return nil, "", err
		}
	}
	return argv, program, nil
}

// Vet splits commands into those the agent may declare and those withheld
// because check failed, each sorted by name.
func Vet(cmds map[string]config.Command, check func(string) (string, error)) ([]proto.CommandDef, []proto.CommandProblem) {
	ok := make(map[string]config.Command, len(cmds))
	problems := []proto.CommandProblem{}
	for _, def := range config.CommandDefs(cmds) { // sorted
		if _, _, err := checkCommand(cmds[def.Name], check); err != nil {
			problems = append(problems, proto.CommandProblem{Name: def.Name, Reason: err.Error()})
			continue
		}
		ok[def.Name] = cmds[def.Name]
	}
	return config.CommandDefs(ok), problems
}
