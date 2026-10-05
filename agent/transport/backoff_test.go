package transport

import (
	"testing"
	"time"
)

func within(d, base time.Duration) bool {
	return d >= time.Duration(float64(base)*0.8) && d <= time.Duration(float64(base)*1.2)
}

func TestBackoffDoublesToCeiling(t *testing.T) {
	b := NewBackoff(time.Second, 60*time.Second, 0.2)
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60}
	for i, w := range want {
		got := b.Next()
		if !within(got, w*time.Second) {
			t.Fatalf("step %d: got %v, want about %v", i, got, w*time.Second)
		}
	}
	b.Reset()
	if got := b.Next(); !within(got, time.Second) {
		t.Fatalf("after reset: %v", got)
	}
	b.Saturate()
	if got := b.Next(); !within(got, 60*time.Second) {
		t.Fatalf("after saturate: %v", got)
	}
}

func TestSleepDetected(t *testing.T) {
	last := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if sleepDetected(last, last.Add(20*time.Second), 15*time.Second, 30*time.Second) {
		t.Fatal("normal tick flagged as sleep")
	}
	if !sleepDetected(last, last.Add(50*time.Second), 15*time.Second, 30*time.Second) {
		t.Fatal("50s jump not flagged")
	}
}
