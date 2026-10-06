//go:build windows

package commands

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func fixedEnv() []string {
	root := os.Getenv("SYSTEMROOT")
	if root == "" {
		root = `C:\Windows`
	}
	return []string{
		"PATH=" + root + `\System32;` + root + `;` + root + `\System32\WindowsPowerShell\v1.0`,
		"SYSTEMROOT=" + root,
		"TEMP=" + os.Getenv("TEMP"),
		"TMP=" + os.Getenv("TMP"),
		"USERPROFILE=" + os.Getenv("USERPROFILE"),
	}
}

// procTree is a command and everything it spawned: on Windows, a Job Object
// with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE. Terminating the job, or closing
// its last handle, ends every process in it, so a timeout or cancel leaves
// no children behind and neither does the agent exiting mid-run.
type procTree struct {
	mu  sync.Mutex
	job windows.Handle
}

// newProcTree creates the job before c starts.
func newProcTree(c *exec.Cmd) (*procTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("configure job object: %w", err)
	}
	return &procTree{job: job}, nil
}

// started puts the new process in the job. Children it spawns afterwards
// join the job automatically; one spawned in the instant before this call
// would not, which os/exec gives no way to close (it cannot start a process
// suspended).
func (t *procTree) started(p *os.Process) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		return fmt.Errorf("open process: %w", err)
	}
	defer windows.CloseHandle(h)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job == 0 {
		return os.ErrProcessDone
	}
	if err := windows.AssignProcessToJobObject(t.job, h); err != nil {
		return fmt.Errorf("assign process to job object: %w", err)
	}
	return nil
}

// terminate ends the job. Windows has no polite stop signal that reaches a
// console-less process tree, so a cancel is immediate here.
func (t *procTree) terminate() error { return t.kill() }

// kill ends every process in the job.
func (t *procTree) kill() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job == 0 {
		return os.ErrProcessDone
	}
	return windows.TerminateJobObject(t.job, 1)
}

// sweep ends anything still in the job; terminate was not polite, so there
// is no grace period to wait out.
func (t *procTree) sweep(time.Time) { t.kill() }

// close releases the job, which also ends anything still running in it.
func (t *procTree) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job != 0 {
		windows.CloseHandle(t.job)
		t.job = 0
	}
}
