//go:build windows

// Windows terminal sessions are not implemented. Decision: ConPTY -> phase 3.1.
// ConPTY from a session-0 service (aymanbagabas/go-pty or UserExistsError/conpty)
// is not quick to land, and phase 3 must not block on it. Until then the agent
// refuses every terminal_open with reason "conpty_not_implemented" before
// spawning anything or dialing the server.

package terminal

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"time"
)

// Supported reports whether this platform can run terminal sessions. When it
// is false, UnsupportedReason says why.
const Supported = false

// UnsupportedReason is why terminal_open is refused on this platform.
const UnsupportedReason = "conpty_not_implemented"

// ErrUnsupported is returned by every entry point on Windows.
var ErrUnsupported = errors.New("terminal: " + UnsupportedReason)

// Session holds everything needed to run a single terminal session.
// On Windows it never opens.
type Session struct {
	URL       string
	TLS       *tls.Config
	SessionID string
	Ticket    string
	Cols      int
	Rows      int
	Log       *slog.Logger
}

// Open always fails on Windows.
func (s *Session) Open(_ context.Context) error { return ErrUnsupported }

// Serve is never reached on Windows because Open fails.
func (s *Session) Serve(_ context.Context) {}

// SelfTest always fails on Windows.
func SelfTest(_ time.Duration) error { return ErrUnsupported }
