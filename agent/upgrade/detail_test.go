package upgrade

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jhyoong/KumaBoard/proto"
)

func TestBuildDetailJoinsAndRedacts(t *testing.T) {
	got := buildDetail("secret-token", "GET /x: status 401", "", "sent Bearer secret-token twice: secret-token")
	want := "GET /x: status 401\nsent Bearer [redacted] twice: [redacted]"
	if got != want {
		t.Fatalf("detail = %q, want %q", got, want)
	}
	// An empty token must not turn every gap into a redaction.
	if got := buildDetail("", "plain"); got != "plain" {
		t.Fatalf("detail with no token = %q", got)
	}
	if got := buildDetail("tok"); got != "" {
		t.Fatalf("detail with no parts = %q", got)
	}
}

func TestBuildDetailDropsInvalidUTF8(t *testing.T) {
	got := buildDetail("", "caf\xc3\xa9 \xff\xfeok")
	if got != "café ok" {
		t.Fatalf("detail = %q", got)
	}
}

func TestBuildDetailCapKeepsFirstLineAndTail(t *testing.T) {
	head := "selftest of 0.2.5 exited 1 after 0.4s"
	// Multi-byte runes so a careless cut would split one.
	stderr := "first stderr line\n" + strings.Repeat("é middle noise\n", 2000) + "error: selftest: config: the real cause"
	footer := "--- stdout (last 1 KiB) ---"
	got := buildDetail("", head, stderr, footer)

	if len(got) > proto.MaxUpgradeDetailLen {
		t.Fatalf("len = %d, over the %d cap", len(got), proto.MaxUpgradeDetailLen)
	}
	if len(got) < proto.MaxUpgradeDetailLen-8 {
		t.Fatalf("len = %d, trimmed far more than needed", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("trim split a rune")
	}
	if !strings.HasPrefix(got, head+"\nfirst stderr line"+trimMarker) {
		t.Fatalf("first lines lost: %q", got[:120])
	}
	if !strings.HasSuffix(got, "error: selftest: config: the real cause\n"+footer) {
		t.Fatalf("tail lost: %q", got[len(got)-120:])
	}
	if strings.Count(got, trimMarker) != 1 {
		t.Fatalf("want exactly one trim marker, got %d", strings.Count(got, trimMarker))
	}
}

func TestBuildDetailCapRedactsBeforeTrimming(t *testing.T) {
	token := "secret-token"
	got := buildDetail(token, strings.Repeat(token+" ", 2000))
	if len(got) > proto.MaxUpgradeDetailLen {
		t.Fatalf("len = %d", len(got))
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "token") {
		t.Fatal("token fragment survived the trim")
	}
}

func TestBuildDetailCapManyParts(t *testing.T) {
	parts := make([]string, 40)
	for i := range parts {
		parts[i] = strings.Repeat("x", 400)
	}
	if got := buildDetail("", parts...); len(got) > proto.MaxUpgradeDetailLen {
		t.Fatalf("len = %d, over the cap", len(got))
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Minute:          "10m",
		15 * time.Second:          "15s",
		time.Hour:                 "1h",
		90 * time.Second:          "1m30s",
		50 * time.Millisecond:     "50ms",
		2*time.Hour + time.Minute: "2h1m",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
