package registry

import (
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

func at(t *testing.T, day string, hm string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04", day+" "+hm, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestInWindowDaily(t *testing.T) {
	sched := store.Schedule{ExpectedOffline: []store.Window{{Days: "*", From: "01:00", To: "05:00"}}, GracePeriodS: 300}
	day := "2026-09-21" // a Monday
	cases := map[string]bool{"00:54": false, "00:56": true, "03:00": true, "05:04": true, "05:06": false, "12:00": false}
	for hm, want := range cases {
		if got := InWindow(at(t, day, hm), sched); got != want {
			t.Errorf("%s: got %v want %v", hm, got, want)
		}
	}
}

func TestInWindowDaysAndWrap(t *testing.T) {
	mon := store.Schedule{ExpectedOffline: []store.Window{{Days: "mon", From: "01:00", To: "05:00"}}}
	if !InWindow(at(t, "2026-09-21", "03:00"), mon) {
		t.Error("Monday 03:00 should match")
	}
	if InWindow(at(t, "2026-09-22", "03:00"), mon) {
		t.Error("Tuesday 03:00 should not match a mon-only window")
	}
	wrap := store.Schedule{ExpectedOffline: []store.Window{{Days: "*", From: "23:00", To: "02:00"}}}
	if !InWindow(at(t, "2026-09-22", "01:00"), wrap) || !InWindow(at(t, "2026-09-21", "23:30"), wrap) {
		t.Error("window crossing midnight not matched")
	}
	if InWindow(at(t, "2026-09-22", "03:00"), wrap) {
		t.Error("03:00 outside 23:00-02:00")
	}
}

func TestDerive(t *testing.T) {
	now := time.Now()
	interval := 30 * time.Second
	sched := store.Schedule{}
	if s := Derive(now, true, now.Add(-10*time.Second), interval, false, sched); s != Online {
		t.Errorf("connected with fresh metrics: %s", s)
	}
	if s := Derive(now, true, now.Add(-2*time.Minute), interval, false, sched); s != Stale {
		t.Errorf("connected with old metrics: %s", s)
	}
	if s := Derive(now, false, now, interval, false, sched); s != OfflineUnexpected {
		t.Errorf("disconnected, no schedule: %s", s)
	}
	if s := Derive(now, false, now, interval, true, sched); s != OfflineExpected {
		t.Errorf("normally_off: %s", s)
	}
}

func TestRing(t *testing.T) {
	r := NewRing(3)
	if r.Latest() != nil || len(r.All()) != 0 {
		t.Fatal("empty ring not empty")
	}
	for i := 1; i <= 4; i++ {
		r.Push(proto.Metrics{UptimeS: uint64(i)})
	}
	all := r.All()
	if len(all) != 3 || all[0].UptimeS != 2 || all[2].UptimeS != 4 || r.Latest().UptimeS != 4 {
		t.Fatalf("ring order wrong: %+v", all)
	}
}
