//go:build windows

package commands

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// Access rights probed by notWritable. Values from winnt.h.
const (
	fileWriteData   = 0x0002 // FILE_WRITE_DATA; on a directory, FILE_ADD_FILE
	fileAppendData  = 0x0004 // FILE_APPEND_DATA; on a directory, FILE_ADD_SUBDIRECTORY
	fileDeleteChild = 0x0040 // FILE_DELETE_CHILD
	rightDelete     = 0x00010000
	rightWriteDAC   = 0x00040000
	rightWriteOwner = 0x00080000
)

// Rights that let an account change what a file contains or who controls it.
var fileRights = []uint32{fileWriteData, fileAppendData, rightDelete, rightWriteDAC, rightWriteOwner}

// Rights that let an account replace an entry in a directory or plant a file
// beside it. FILE_ADD_SUBDIRECTORY is deliberately absent: a default C:\
// grants it to every authenticated user, so probing it would fail every
// path on the system drive, and adding a subdirectory cannot replace an
// existing entry on the path being checked.
var dirRights = []uint32{fileWriteData, fileDeleteChild, rightDelete, rightWriteDAC, rightWriteOwner}

// notWritable fails when this process could modify p. Instead of parsing the
// DACL it asks the kernel for effective access: it tries to open p once per
// right and treats any success as writable. Nothing is written; the handle
// is closed at once.
func (c *fileChecker) notWritable(p string, fi os.FileInfo) error {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return fmt.Errorf("%s: %v", p, err)
	}
	rights := fileRights
	// Do not follow a reparse point; the walk resolves links itself.
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if fi.IsDir() {
		rights = dirRights
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS // required to open a directory
	}
	const share = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE
	for _, right := range rights {
		h, err := windows.CreateFile(name, right, share, nil, windows.OPEN_EXISTING, flags, 0)
		if err == nil {
			windows.CloseHandle(h)
			return fmt.Errorf("%s is writable by the agent account", p)
		}
		switch {
		case errors.Is(err, windows.ERROR_ACCESS_DENIED), errors.Is(err, windows.ERROR_WRITE_PROTECT):
			// Not granted; try the next right.
		case errors.Is(err, windows.ERROR_SHARING_VIOLATION), errors.Is(err, windows.ERROR_LOCK_VIOLATION),
			errors.Is(err, windows.ERROR_USER_MAPPED_FILE):
			// The access check passed and only a sharing rule refused the
			// open, so the right is held.
			return fmt.Errorf("%s is writable by the agent account", p)
		default:
			// Fail closed on anything unexpected.
			return fmt.Errorf("%s: cannot check access: %v", p, err)
		}
	}
	return nil
}
