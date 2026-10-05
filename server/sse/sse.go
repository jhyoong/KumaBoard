// Package sse fans dashboard events out to browsers over Server-Sent Events.
package sse

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Event is one named JSON payload.
type Event struct {
	Name string
	Data any
}

// Broker holds subscribers. Slow subscribers drop events rather than block
// publishers; the dashboard refetches on reconnect anyway.
type Broker struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// New creates a broker.
func New() *Broker {
	return &Broker{subs: map[chan Event]struct{}{}}
}

// Publish sends an event to every subscriber.
func (b *Broker) Publish(name string, data any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- Event{Name: name, Data: data}:
		default:
		}
	}
}

// Subscribe registers a subscriber. Call cancel to leave.
func (b *Broker) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// ServeHTTP streams events until the client disconnects.
func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ch, cancel := b.Subscribe()
	defer cancel()
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case ev := <-ch:
			data, err := json.Marshal(ev.Data)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, data)
			flusher.Flush()
		}
	}
}
