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

func TestCommandEventsAndSummaryFields(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	ctx := context.Background()
	d, _, _ := st.CreateDevice(ctx, "a", "", false, store.Schedule{})
	pub := &capture{}
	r := New(st, pub, 30*time.Second)
	r.Load(ctx)

	// Never null in JSON: the dashboard maps over both lists.
	s, _ := r.Summary(ctx, "a")
	if s.Commands == nil || s.CommandProblems == nil || s.CommandsConfigError != "" {
		t.Fatalf("empty device: %+v", s)
	}

	st.ReplaceCommands(ctx, d.ID,
		[]proto.CommandDef{{Name: "backup", TimeoutS: 60, Confirm: true}},
		[]proto.CommandProblem{{Name: "wipe", Reason: "/opt/x is writable by the agent account"}}, "yaml: line 3")
	r.CommandsChanged("a")
	if pub.count("device") != 1 {
		t.Fatalf("events: %v", pub.events)
	}
	s, _ = r.Summary(ctx, "a")
	if len(s.Commands) != 1 || !s.Commands[0].Confirm || len(s.CommandProblems) != 1 || s.CommandProblems[0].Name != "wipe" || s.CommandsConfigError != "yaml: line 3" {
		t.Fatalf("summary: %+v", s)
	}
	sums, _ := r.Summaries(ctx)
	if len(sums) != 1 || len(sums[0].CommandProblems) != 1 {
		t.Fatalf("summaries: %+v", sums)
	}

	r.RunOutput("run1", "a", 3, proto.CommandOutput{Stream: proto.StreamStdout, Data: "hi\n"})
	if pub.count("run_output") != 1 {
		t.Fatalf("events: %v", pub.events)
	}
}
