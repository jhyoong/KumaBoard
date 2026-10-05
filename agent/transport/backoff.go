package transport

import (
	"math/rand/v2"
	"time"
)

// Backoff doubles from Min to Max with symmetric jitter.
type Backoff struct {
	min, max time.Duration
	jitter   float64
	cur      time.Duration
}

// NewBackoff creates a backoff. jitter is a fraction, e.g. 0.2 for plus or minus 20 percent.
func NewBackoff(min, max time.Duration, jitter float64) *Backoff {
	return &Backoff{min: min, max: max, jitter: jitter}
}

// Next returns the next delay and advances.
func (b *Backoff) Next() time.Duration {
	if b.cur == 0 {
		b.cur = b.min
	} else {
		b.cur *= 2
		if b.cur > b.max {
			b.cur = b.max
		}
	}
	f := 1 + (rand.Float64()*2-1)*b.jitter
	return time.Duration(float64(b.cur) * f)
}

// Reset returns to the minimum delay.
func (b *Backoff) Reset() { b.cur = 0 }

// Saturate jumps to the ceiling. Used after a handshake rejection.
func (b *Backoff) Saturate() { b.cur = b.max }

// sleepDetected reports whether wall-clock time jumped more than the tick
// interval plus the allowed slack, which means the machine was asleep.
func sleepDetected(last, now time.Time, interval, slack time.Duration) bool {
	return now.Sub(last) > interval+slack
}
