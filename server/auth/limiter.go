package auth

import (
	"net/netip"
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

// clientKey is the bucket an address counts against. An IPv6 client usually
// holds a whole /64 and can pick a new address in it per request, so IPv6 is
// keyed by that prefix.
func clientKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is6() {
		if p, err := addr.Prefix(64); err == nil {
			return p.String()
		}
	}
	return addr.String()
}

// Allowed reports whether ip is currently free to attempt a login. It does
// not count an attempt; see Reserve.
func (l *Limiter) Allowed(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.allowed(clientKey(ip))
}

func (l *Limiter) allowed(key string) bool {
	b, ok := l.entries[key]
	if !ok {
		return true
	}
	if !b.lockedUntil.IsZero() {
		if l.now().Before(b.lockedUntil) {
			return false
		}
		delete(l.entries, key)
	}
	return true
}

// Reserve counts one attempt from ip as failed, before its outcome is known,
// and reports whether the attempt may go ahead. Counting up front is what
// stops parallel requests from all passing the check before any of them has
// recorded a failure. A successful login calls Reset.
func (l *Limiter) Reserve(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := clientKey(ip)
	if !l.allowed(key) {
		return false
	}
	l.fail(key)
	return true
}

// Fail records a failed attempt from ip.
func (l *Limiter) Fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fail(clientKey(ip))
}

func (l *Limiter) fail(key string) {
	now := l.now()
	b, ok := l.entries[key]
	if !ok {
		b = &bucket{}
		l.entries[key] = b
	}
	// Failures older than the lockout window no longer count.
	if now.Sub(b.lastFail) > l.lockout {
		b.failures = 0
	}
	b.failures++
	b.lastFail = now
	if b.failures >= l.max {
		b.failures = 0
		b.lockedUntil = now.Add(l.lockout)
	}
}

func (l *Limiter) Reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, clientKey(ip))
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
