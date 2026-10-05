package auth

import (
	"sync"
	"time"
)

type Limiter struct {
	max     int
	lockout time.Duration
	now     func() time.Time

	mu      sync.Mutex
	entries map[string]*bucket
}

type bucket struct {
	failures    int
	lastFail    time.Time
	lockedUntil time.Time
}

func NewLimiter(max int, lockout time.Duration) *Limiter {
	return &Limiter{max: max, lockout: lockout, now: time.Now, entries: map[string]*bucket{}}
}

func (l *Limiter) Allowed(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.entries[ip]
	if !ok {
		return true
	}
	if !b.lockedUntil.IsZero() {
		if l.now().Before(b.lockedUntil) {
			return false
		}
		delete(l.entries, ip)
	}
	return true
}

func (l *Limiter) Fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.entries[ip]
	if !ok {
		b = &bucket{}
		l.entries[ip] = b
	}
	b.failures++
	b.lastFail = l.now()
	if b.failures >= l.max {
		b.failures = 0
		b.lockedUntil = l.now().Add(l.lockout)
	}
}

func (l *Limiter) Reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, ip)
}

// Sweep removes expired lockouts and stale failure records.
func (l *Limiter) Sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-l.lockout)
	for ip, b := range l.entries {
		if !b.lockedUntil.IsZero() && now.After(b.lockedUntil) {
			delete(l.entries, ip)
		} else if b.lockedUntil.IsZero() && b.lastFail.Before(cutoff) {
			delete(l.entries, ip)
		}
	}
}
