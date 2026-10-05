package registry

import (
	"fmt"
	"strings"
	"time"

	"github.com/jhyoong/KumaBoard/server/store"
)

// State is a device's derived status.
type State string

const (
	Online            State = "online"
	Stale             State = "stale"
	OfflineExpected   State = "offline_expected"
	OfflineUnexpected State = "offline_unexpected"
)

var dayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

func parseHM(s string) (int, bool) {
	var h, m int
	if n, err := fmt.Sscanf(s, "%d:%d", &h, &m); n != 2 || err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

func dayMatches(spec string, d time.Weekday) bool {
	if strings.TrimSpace(spec) == "*" || spec == "" {
		return true
	}
	for _, part := range strings.Split(spec, ",") {
		if wd, ok := dayNames[strings.ToLower(strings.TrimSpace(part))]; ok && wd == d {
			return true
		}
	}
	return false
}

// InWindow reports whether now falls inside any expected-offline window,
// widened by the grace period on both sides.
func InWindow(now time.Time, sched store.Schedule) bool {
	grace := time.Duration(sched.GracePeriodS) * time.Second
	for _, w := range sched.ExpectedOffline {
		from, ok1 := parseHM(w.From)
		to, ok2 := parseHM(w.To)
		if !ok1 || !ok2 {
			continue
		}
		dur := time.Duration(to-from) * time.Minute
		if dur <= 0 {
			dur += 24 * time.Hour
		}
		for _, dayOffset := range []int{0, -1} {
			day := time.Date(now.Year(), now.Month(), now.Day()+dayOffset, 0, 0, 0, 0, now.Location())
			if !dayMatches(w.Days, day.Weekday()) {
				continue
			}
			start := day.Add(time.Duration(from) * time.Minute)
			end := start.Add(dur)
			if !now.Before(start.Add(-grace)) && now.Before(end.Add(grace)) {
				return true
			}
		}
	}
	return false
}

// Derive computes the state from the raw facts.
func Derive(now time.Time, connected bool, lastMetrics time.Time, interval time.Duration, normallyOff bool, sched store.Schedule) State {
	if connected {
		if now.Sub(lastMetrics) > 3*interval {
			return Stale
		}
		return Online
	}
	if normallyOff || InWindow(now, sched) {
		return OfflineExpected
	}
	return OfflineUnexpected
}
