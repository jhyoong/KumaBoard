//go:build !windows

package collectors

import "errors"

const rootPath = "/"

var errNoCollectors = errors.New("collectors: every collector failed")
