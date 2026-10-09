package proto

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

const testUpgradeID = "01J9ZQ4T6M8N2P3R5S7V9W0XYZ"

func TestUpgradeResultRoundTrip(t *testing.T) {
	for _, in := range []UpgradeResult{
		{FromVersion: "0.2.0", ToVersion: "0.3.0", State: UpgradeDownloading},
		{FromVersion: "0.2.0", ToVersion: "0.3.0", State: UpgradeFailed,
			Reason: UpgradeReasonSelftestConfigRejected, UpgradeID: testUpgradeID,
			Detail: "selftest: config: bad key\n\texit status 1"},
	} {
		env, err := New(TypeUpgradeResult, in)
		if err != nil {
			t.Fatal(err)
		}
		var out UpgradeResult
		if err := env.Unmarshal(&out); err != nil {
			t.Fatal(err)
		}
		if out != in {
			t.Fatalf("round trip: got %+v want %+v", out, in)
		}
	}
}

func TestUpgradeNewFieldsOmittedWhenEmpty(t *testing.T) {
	b, err := json.Marshal(UpgradeResult{FromVersion: "a", ToVersion: "b", State: UpgradeVerified})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Contains(s, "upgrade_id") || strings.Contains(s, "detail") || strings.Contains(s, "reason") {
		t.Fatalf("empty fields on the wire: %s", s)
	}
	b, err = json.Marshal(UpgradeRequest{Version: "0.3.0"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "upgrade_id") {
		t.Fatalf("empty upgrade_id on the wire: %s", b)
	}
}

func TestUpgradeWithoutNewFieldsDecodes(t *testing.T) {
	// A result from an agent that predates upgrade_id and detail.
	var res UpgradeResult
	old := []byte(`{"from_version":"0.2.0","to_version":"0.3.0","state":"failed","reason":"verify_failed"}`)
	if err := json.Unmarshal(old, &res); err != nil {
		t.Fatal(err)
	}
	want := UpgradeResult{FromVersion: "0.2.0", ToVersion: "0.3.0", State: UpgradeFailed, Reason: UpgradeReasonVerifyFailed}
	if res != want {
		t.Fatalf("got %+v", res)
	}
	if !res.Sanitize() || res != want {
		t.Fatalf("sanitize changed an old result: %+v", res)
	}

	// A request from a server that predates upgrade_id.
	var req UpgradeRequest
	oldReq := []byte(`{"version":"0.3.0","os":"linux","arch":"amd64","sha256":"ab","size_bytes":7,"signature":"s","url":"/u"}`)
	if err := json.Unmarshal(oldReq, &req); err != nil {
		t.Fatal(err)
	}
	if req.UpgradeID != "" || req.SizeBytes != 7 || req.URL != "/u" {
		t.Fatalf("got %+v", req)
	}
}

func TestUpgradeRequestRoundTrip(t *testing.T) {
	in := UpgradeRequest{Version: "0.3.0", OS: "linux", Arch: "amd64", SHA256: "ab",
		SizeBytes: 7, Signature: "s", URL: "/u", UpgradeID: testUpgradeID}
	env, err := New(TypeUpgradeRequest, in)
	if err != nil {
		t.Fatal(err)
	}
	var out UpgradeRequest
	if err := env.Unmarshal(&out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("round trip: got %+v want %+v", out, in)
	}
}

func TestValidUpgradeState(t *testing.T) {
	for _, s := range []string{
		UpgradeRequested, UpgradeDownloading, UpgradeVerifying, UpgradeSelftest, UpgradeSwapped,
		UpgradeRestarting, UpgradeVerified, UpgradeRolledBack, UpgradeFailed,
	} {
		if !ValidUpgradeState(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range []string{"", "Failed", "failed ", "abandoned", "rolled-back", "verified\n"} {
		if ValidUpgradeState(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
}

func TestUpgradeSanitizeState(t *testing.T) {
	ok := UpgradeResult{State: UpgradeFailed}
	if !ok.Sanitize() {
		t.Fatal("known state rejected")
	}
	bad := UpgradeResult{State: "exploded", Reason: "Bad Reason", UpgradeID: "x", Detail: "a\x1b[31mb"}
	if bad.Sanitize() {
		t.Fatal("unknown state accepted")
	}
	// State is kept for logging; the rest is cleaned regardless.
	if bad.State != "exploded" || bad.Reason != UpgradeReasonInvalid || bad.UpgradeID != "" || bad.Detail != "a[31mb" {
		t.Fatalf("got %+v", bad)
	}
}

func TestUpgradeSanitizeReason(t *testing.T) {
	cases := map[string]string{
		"":                       "",
		"busy":                   "busy",
		"crash_loop":             "crash_loop",
		"http_404":               "http_404",
		strings.Repeat("a", 64):  strings.Repeat("a", 64),
		strings.Repeat("a", 65):  UpgradeReasonInvalid,
		"Verify_Failed":          UpgradeReasonInvalid,
		"verify failed":          UpgradeReasonInvalid,
		"verify-failed":          UpgradeReasonInvalid,
		"busy\n":                 UpgradeReasonInvalid,
		"<script>":               UpgradeReasonInvalid,
		"défaut":                 UpgradeReasonInvalid,
		"\x1b[31mfailed":         UpgradeReasonInvalid,
		UpgradeReasonInvalid:     UpgradeReasonInvalid,
		UpgradeReasonNoHandshake: UpgradeReasonNoHandshake,
	}
	for in, want := range cases {
		r := UpgradeResult{State: UpgradeFailed, Reason: in}
		r.Sanitize()
		if r.Reason != want {
			t.Errorf("reason %q: got %q want %q", in, r.Reason, want)
		}
	}
	for _, reason := range []string{
		UpgradeReasonDownloadFailed, UpgradeReasonVerifyFailed, UpgradeReasonSelftestFailed,
		UpgradeReasonSwapFailed, UpgradeReasonNoHandshake, UpgradeReasonSelftestConfigRejected,
		UpgradeReasonStartupFailed, UpgradeReasonCrashLoop, UpgradeReasonBusy,
	} {
		r := UpgradeResult{State: UpgradeFailed, Reason: reason}
		r.Sanitize()
		if r.Reason != reason {
			t.Errorf("constant %q did not survive: %q", reason, r.Reason)
		}
	}
}

func TestUpgradeSanitizeUpgradeID(t *testing.T) {
	// The format the server stores in agent_upgrades.id.
	env, err := New(TypeUpgradeResult, UpgradeResult{})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"":                             "",
		testUpgradeID:                  testUpgradeID,
		env.ID:                         env.ID,
		"7ZZZZZZZZZZZZZZZZZZZZZZZZZ":   "7ZZZZZZZZZZZZZZZZZZZZZZZZZ",
		"8ZZZZZZZZZZZZZZZZZZZZZZZZZ":   "", // overflows 128 bits
		testUpgradeID[:25]:             "",
		testUpgradeID + "0":            "",
		strings.ToLower(testUpgradeID): "",
		"01J9ZQ4T6M8N2P3R5S7V9W0XYI":   "", // I, L, O, U are not in the alphabet
		"01J9ZQ4T6M8N2P3R5S7V9W0XYU":   "",
		"01J9ZQ4T6M8N2P3R5S7V9W0X-Z":   "",
		"01J9ZQ4T6M8N2P3R5S7V9W0X\nZ":  "",
		"' OR 1=1 --               ":   "",
		"3f2b8c1e-9d4a-4e6f-8a1b-2c3d": "",
	}
	for in, want := range cases {
		r := UpgradeResult{State: UpgradeFailed, UpgradeID: in}
		r.Sanitize()
		if r.UpgradeID != want {
			t.Errorf("id %q: got %q want %q", in, r.UpgradeID, want)
		}
	}
}

func TestUpgradeSanitizeDetail(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"empty", "", ""},
		{"plain", "exit status 1", "exit status 1"},
		{"newline and tab kept", "a\n\tb", "a\n\tb"},
		{"crlf folded", "a\r\nb\r\n", "a\nb\n"},
		{"lone cr removed", "a\rb", "ab"},
		{"ansi escape", "\x1b[31mred\x1b[0m", "[31mred[0m"},
		{"c0 controls", "a\x00b\x07c\x08d\x0be\x0cf\x7fg", "abcdefg"},
		{"c1 controls", "a\u0085b\u009bc", "abc"},
		{"invalid utf8", "a\xffb\xc3(c\xe2\x82", "ab(c"},
		{"multibyte kept", "défaut 日本語 \U0001f43b", "défaut 日本語 \U0001f43b"},
		{"cr exposed by invalid byte", "a\r\xff\nb", "a\nb"},
	}
	for _, c := range cases {
		r := UpgradeResult{State: UpgradeFailed, Detail: c.in}
		r.Sanitize()
		if r.Detail != c.want {
			t.Errorf("%s: got %q want %q", c.name, r.Detail, c.want)
		}
		before := r
		r.Sanitize()
		if r != before {
			t.Errorf("%s: not idempotent: %q then %q", c.name, before.Detail, r.Detail)
		}
	}
}

func TestUpgradeSanitizeDetailCut(t *testing.T) {
	// Exactly at the cap: untouched.
	r := UpgradeResult{State: UpgradeFailed, Detail: strings.Repeat("x", MaxUpgradeDetailLen)}
	r.Sanitize()
	if len(r.Detail) != MaxUpgradeDetailLen {
		t.Fatalf("len %d", len(r.Detail))
	}

	// ASCII over the cap: cut to the cap.
	r.Detail = strings.Repeat("x", MaxUpgradeDetailLen+100)
	r.Sanitize()
	if len(r.Detail) != MaxUpgradeDetailLen {
		t.Fatalf("len %d", len(r.Detail))
	}

	// A 3-byte rune straddles the cap at each possible offset: the cut backs
	// up to the rune boundary instead of splitting it.
	for pad := 0; pad < 3; pad++ {
		r.Detail = strings.Repeat("x", pad) + strings.Repeat("日", MaxUpgradeDetailLen)
		r.Sanitize()
		want := pad + (MaxUpgradeDetailLen-pad)/3*3
		if len(r.Detail) != want || !utf8.ValidString(r.Detail) {
			t.Fatalf("pad %d: len %d want %d, valid %v", pad, len(r.Detail), want, utf8.ValidString(r.Detail))
		}
	}

	// The cap applies after cleaning, so removed bytes do not count.
	r.Detail = strings.Repeat("\x1b", 5000) + strings.Repeat("y", MaxUpgradeDetailLen)
	r.Sanitize()
	if r.Detail != strings.Repeat("y", MaxUpgradeDetailLen) {
		t.Fatalf("len %d", len(r.Detail))
	}
}

func TestUpgradeVersionUnchanged(t *testing.T) {
	// The new fields are additive; they must not move the version window.
	if Version != 2 || MinSupported != 1 {
		t.Fatalf("Version=%d MinSupported=%d", Version, MinSupported)
	}
}
