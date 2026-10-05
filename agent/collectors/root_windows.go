//go:build windows

package collectors

import "errors"

const rootPath = `C:\`

var errNoCollectors = errors.New("collectors: every collector failed")
