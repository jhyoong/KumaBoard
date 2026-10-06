package registry

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
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

func TestSummaryUpgradeFields(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	ctx := context.Background()
	d, _, _ := st.CreateDevice(ctx, "a", "", false, store.Schedule{})
	pub := &capture{}
	r := New(st, pub, 30*time.Second)
	r.Load(ctx)

	// Nothing to report: both are null in JSON.
	s, _ := r.Summary(ctx, "a")
	if s.UpgradeDispatch != nil || s.LatestUpgrade != nil {
		t.Fatalf("empty device: %+v", s)
	}
	b, _ := json.Marshal(s)
	var raw map[string]json.RawMessage
	json.Unmarshal(b, &raw)
	if string(raw["upgrade_dispatch"]) != "null" || string(raw["latest_upgrade"]) != "null" {
		t.Fatalf("json = %s", b)
	}

	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	st.SetUpgradeDispatch(ctx, d.ID, store.UpgradeDispatch{Code: store.DispatchSent, Message: "upgrade request sent", UpgradeID: "01J", At: at})
	id, _ := st.CreateUpgrade(ctx, d.ID, "0.2.4", "0.2.5", "admin")
	st.RecordUpgradeEvent(ctx, store.UpgradeEvent{UpgradeID: id, Source: store.UpgradeSourceAgent, State: proto.UpgradeRestarting})
	u, _ := st.GetUpgrade(ctx, id)

	s, _ = r.Summary(ctx, "a")
	if s.UpgradeDispatch == nil || s.UpgradeDispatch.Code != store.DispatchSent || s.UpgradeDispatch.UpgradeID != "01J" {
		t.Fatalf("dispatch = %+v", s.UpgradeDispatch)
	}
	lu := s.LatestUpgrade
	if lu == nil || lu.ID != id || lu.FromVersion != "0.2.4" || lu.ToVersion != "0.2.5" || lu.State != proto.UpgradeRestarting ||
		lu.FailureReason != "" || !lu.UpdatedAt.Equal(u.UpdatedAt) || lu.Stalled {
		t.Fatalf("latest = %+v", lu)
	}
	// Exactly the documented fields, and no detail text.
	b, _ = json.Marshal(s)
	json.Unmarshal(b, &raw)
	var latest, dispatch map[string]any
	json.Unmarshal(raw["latest_upgrade"], &latest)
	json.Unmarshal(raw["upgrade_dispatch"], &dispatch)
	if got := keys(latest); !slices.Equal(got, []string{"failure_reason", "from_version", "id", "stalled", "state", "to_version", "updated_at"}) {
		t.Fatalf("latest_upgrade keys = %v", got)
	}
	if got := keys(dispatch); !slices.Equal(got, []string{"at", "blocking_device", "code", "message", "upgrade_id"}) {
		t.Fatalf("upgrade_dispatch keys = %v", got)
	}
	if dispatch["at"] != "2026-10-06T12:00:00Z" {
		t.Fatalf("at = %v", dispatch["at"])
	}

	// Stalled is computed when the summary is built, so a republish alone
	// turns the flag on.
	r.now = func() time.Time { return u.UpdatedAt.Add(5*time.Minute + time.Second) }
	if s, _ = r.Summary(ctx, "a"); !s.LatestUpgrade.Stalled {
		t.Fatalf("not stalled: %+v", s.LatestUpgrade)
	}
	if sums, _ := r.Summaries(ctx); len(sums) != 1 || sums[0].LatestUpgrade == nil || !sums[0].LatestUpgrade.Stalled {
		t.Fatalf("summaries = %+v", sums)
	}
	n := pub.count("device")
	r.UpgradeChanged("a")
	if pub.count("device") != n+1 {
		t.Fatalf("events: %v", pub.events)
	}

	// The latest upgrade is the newest one; its reason comes along.
	st.RecordUpgradeEvent(ctx, store.UpgradeEvent{UpgradeID: id, Source: store.UpgradeSourceAgent, State: proto.UpgradeRolledBack,
		Reason: proto.UpgradeReasonNoHandshake, Detail: "long agent text"})
	s, _ = r.Summary(ctx, "a")
	if lu := s.LatestUpgrade; lu.State != proto.UpgradeRolledBack || lu.FailureReason != proto.UpgradeReasonNoHandshake || lu.Stalled {
		t.Fatalf("latest = %+v", lu)
	}
	if b, _ := json.Marshal(s); strings.Contains(string(b), "long agent text") {
		t.Fatalf("summary carries detail text: %s", b)
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
