package upgrade

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jhyoong/KumaBoard/proto"
)

// trimMarker replaces the middle of a part that buildDetail had to shorten.
const trimMarker = "\n[... trimmed ...]\n"

// buildDetail builds the free-text detail of an upgrade result. It joins the
// non-empty parts with newlines, replaces every occurrence of the bearer
// token with [redacted], drops invalid UTF-8, and keeps the result within
// proto.MaxUpgradeDetailLen by trimming the middle of the longest part: the
// first line and the tail are what matter.
//
// Parts are error values and selftest output only. Nothing here reads a file.
func buildDetail(token string, parts ...string) string {
	kept := parts[:0:0]
	for _, p := range parts {
		if token != "" {
			p = strings.ReplaceAll(p, token, "[redacted]")
		}
		if p = strings.ToValidUTF8(p, ""); p != "" {
			kept = append(kept, p)
		}
	}
	// One pass is enough unless the excess is spread over several parts.
	for range kept {
		excess := joinedLen(kept) - proto.MaxUpgradeDetailLen
		if excess <= 0 {
			break
		}
		longest := 0
		for i, p := range kept {
			if len(p) > len(kept[longest]) {
				longest = i
			}
		}
		kept[longest] = trimMiddle(kept[longest], len(kept[longest])-excess)
	}
	s := strings.Join(kept, "\n")
	if len(s) > proto.MaxUpgradeDetailLen {
		s = s[:runeStartBefore(s, proto.MaxUpgradeDetailLen)]
	}
	return s
}

func joinedLen(parts []string) int {
	n := len(parts) - 1
	for _, p := range parts {
		n += len(p)
	}
	return n
}

// trimMiddle shortens s to at most limit bytes, keeping its first line (when
// that fits in half the budget) and as much of its tail as possible.
func trimMiddle(s string, limit int) string {
	keep := limit - len(trimMarker)
	if keep <= 0 {
		return s[:runeStartBefore(s, max(limit, 0))]
	}
	head := keep / 4
	if nl := strings.IndexByte(s, '\n'); nl >= 0 && nl <= keep/2 {
		head = nl
	}
	head = runeStartBefore(s, head)
	tail := len(s) - (keep - head)
	for tail < len(s) && !utf8.RuneStart(s[tail]) {
		tail++
	}
	return s[:head] + trimMarker + s[tail:]
}

// runeStartBefore returns the largest index <= n that does not split a rune.
func runeStartBefore(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}

// tailBytes returns the last n bytes of s without splitting a rune.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

// shortDuration prints d without trailing zero units: 10m, not 10m0s.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
