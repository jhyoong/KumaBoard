package registry

import "github.com/jhyoong/KumaBoard/proto"

// Ring holds the last N metrics samples for a device.
type Ring struct {
	buf  []proto.Metrics
	n    int
	next int
}

// NewRing creates a ring of the given capacity.
func NewRing(size int) *Ring {
	return &Ring{buf: make([]proto.Metrics, size)}
}

// Push adds a sample, evicting the oldest when full.
func (r *Ring) Push(m proto.Metrics) {
	r.buf[r.next] = m
	r.next = (r.next + 1) % len(r.buf)
	if r.n < len(r.buf) {
		r.n++
	}
}

// Latest returns the newest sample or nil.
func (r *Ring) Latest() *proto.Metrics {
	if r.n == 0 {
		return nil
	}
	m := r.buf[(r.next-1+len(r.buf))%len(r.buf)]
	return &m
}

// All returns samples oldest first.
func (r *Ring) All() []proto.Metrics {
	out := make([]proto.Metrics, 0, r.n)
	start := (r.next - r.n + len(r.buf)) % len(r.buf)
	for i := 0; i < r.n; i++ {
		out = append(out, r.buf[(start+i)%len(r.buf)])
	}
	return out
}
