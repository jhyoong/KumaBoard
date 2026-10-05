package registry

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

type capture struct {
	mu     sync.Mutex
	events []string
}

func (c *capture) Publish(name string, data any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, name)
}

func (c *capture) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.events {
		if e == name {
			n++
		}
	}
	return n
}

func TestRegistryFlow(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	st.CreateDevice(ctx, "a", "", false, store.Schedule{})
	st.CreateDevice(ctx, "b", "", true, store.Schedule{})
	pub := &capture{}
	r := New(st, pub, 30*time.Second)
	if err := r.Load(ctx); err != nil {
		t.Fatal(err)
	}
	sums, _ := r.Summaries(ctx)
	if len(sums) != 2 || sums[0].State != OfflineUnexpected || sums[1].State != OfflineExpected {
		t.Fatalf("initial states: %+v", sums)
	}
	r.DeviceConnected("a")
	s, _ := r.Summary(ctx, "a")
	if s.State != Online || !s.Connected {
		t.Fatalf("after connect: %+v", s)
	}
	r.MetricsReceived("a", proto.Metrics{CPUPercent: 5})
	s, _ = r.Summary(ctx, "a")
	if s.Metrics == nil || s.Metrics.CPUPercent != 5 || len(r.Metrics("a")) != 1 {
		t.Fatalf("metrics not stored: %+v", s)
	}
	r.DeviceDisconnected("a")
	s, _ = r.Summary(ctx, "a")
	if s.State != OfflineUnexpected {
		t.Fatalf("after disconnect: %+v", s)
	}
	if pub.count("device") < 3 || pub.count("metrics") != 1 {
		t.Fatalf("events: %v", pub.events)
	}
}

func TestSummaryFlagsIncompatible(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	ctx := context.Background()
	st.CreateDevice(ctx, "a", "", false, store.Schedule{})
	st.RecordReject(ctx, "a", proto.ErrProtocolVersionUnsupported)
	r := New(st, &capture{}, 30*time.Second)
	r.Load(ctx)
	s, _ := r.Summary(ctx, "a")
	if !s.Incompatible {
		t.Fatalf("expected incompatible flag: %+v", s)
	}
}
