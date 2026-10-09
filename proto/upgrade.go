package proto

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Upgrade messages are frozen at protocol v1. Fields may be added; existing
// fields never change meaning and are never removed.

// UpgradeRequest asks the agent to move to a target version.
type UpgradeRequest struct {
	Version   string `json:"version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	Signature string `json:"signature"`
	URL       string `json:"url"`
	// UpgradeID is the server's agent_upgrades.id. The agent echoes it in
	// every UpgradeResult for this attempt. Empty from older servers.
	UpgradeID string `json:"upgrade_id,omitempty"`
}

// Upgrade states. UpgradeRequested is set by the server when it creates the
// attempt; the agent reports the rest.
const (
	UpgradeRequested   = "requested"
	UpgradeDownloading = "downloading"
	UpgradeVerifying   = "verifying"
	UpgradeSelftest    = "selftest"
	UpgradeSwapped     = "swapped"
	UpgradeRestarting  = "restarting"
	UpgradeVerified    = "verified"
	UpgradeRolledBack  = "rolled_back"
	UpgradeFailed      = "failed"
)

// Upgrade failure reasons.
const (
	UpgradeReasonDownloadFailed = "download_failed"
	UpgradeReasonVerifyFailed   = "verify_failed"
	UpgradeReasonSelftestFailed = "selftest_failed"
	UpgradeReasonSwapFailed     = "swap_failed"
	UpgradeReasonNoHandshake    = "no_handshake"

	// The candidate binary rejected the agent config during selftest.
	UpgradeReasonSelftestConfigRejected = "selftest_config_rejected"
	// The new binary started but failed before it could connect.
	UpgradeReasonStartupFailed = "startup_failed"
	// The new binary restarted too many times during probation.
	UpgradeReasonCrashLoop = "crash_loop"
	// Another upgrade is already running on the agent.
	UpgradeReasonBusy = "busy"

	// UpgradeReasonInvalid replaces a reason that fails Sanitize.
	UpgradeReasonInvalid = "invalid_reason"
)

// MaxUpgradeDetailLen caps UpgradeResult.Detail, in bytes.
const MaxUpgradeDetailLen = 8192

// maxUpgradeReasonLen caps UpgradeResult.Reason, in bytes.
const maxUpgradeReasonLen = 64

// ulidLen is the length of a ULID in its canonical text form.
const ulidLen = 26

// UpgradeResult reports progress or outcome of an upgrade.
type UpgradeResult struct {
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
	State       string `json:"state"`
	Reason      string `json:"reason,omitempty"`
	// UpgradeID echoes UpgradeRequest.UpgradeID. Empty from older agents.
	UpgradeID string `json:"upgrade_id,omitempty"`
	// Detail is free text explaining a failure, at most MaxUpgradeDetailLen
	// bytes. It is untrusted: receivers call Sanitize before using it.
	Detail string `json:"detail,omitempty"`
}

// ValidUpgradeState reports whether s is a known upgrade state.
func ValidUpgradeState(s string) bool {
	switch s {
	case UpgradeRequested, UpgradeDownloading, UpgradeVerifying, UpgradeSelftest,
		UpgradeSwapped, UpgradeRestarting, UpgradeVerified, UpgradeRolledBack, UpgradeFailed:
		return true
	}
	return false
}

// Sanitize cleans a result received from a peer, in place, and reports
// whether State is a known upgrade state. A false return means the message
// must be dropped; State is left as received so the caller can log it, and
// the other fields are still cleaned.
//
//   - Reason must match ^[a-z0-9_]{1,64}$ or becomes UpgradeReasonInvalid.
//     An empty reason stays empty.
//   - UpgradeID must look like a ULID as the server generates them (26
//     upper-case Crockford base32 characters) or becomes empty.
//   - Detail has invalid UTF-8 removed, CR LF folded to LF, every control
//     character except \n and \t removed (which defuses ANSI escapes), and
//     is cut to MaxUpgradeDetailLen bytes on a rune boundary.
//
// FromVersion and ToVersion are not touched. Sanitize is idempotent.
func (r *UpgradeResult) Sanitize() bool {
	if r.Reason != "" && !validUpgradeReason(r.Reason) {
		r.Reason = UpgradeReasonInvalid
	}
	if !looksLikeULID(r.UpgradeID) {
		r.UpgradeID = ""
	}
	r.Detail = sanitizeUpgradeDetail(r.Detail)
	return ValidUpgradeState(r.State)
}

func validUpgradeReason(s string) bool {
	if s == "" || len(s) > maxUpgradeReasonLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// looksLikeULID checks the shape only: length, alphabet, and a first
// character that keeps the value within 128 bits.
func looksLikeULID(s string) bool {
	if len(s) != ulidLen || s[0] > '7' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'A' && c <= 'Z' && c != 'I' && c != 'L' && c != 'O' && c != 'U':
		default:
			return false
		}
	}
	return true
}

func sanitizeUpgradeDetail(s string) string {
	if s == "" {
		return s
	}
	s = strings.ToValidUTF8(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.Map(func(c rune) rune {
		if c != '\n' && c != '\t' && unicode.IsControl(c) {
			return -1
		}
		return c
	}, s)
	if len(s) > MaxUpgradeDetailLen {
		n := MaxUpgradeDetailLen
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n]
	}
	return s
}
