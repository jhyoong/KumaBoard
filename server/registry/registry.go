// Package registry holds live device state derived from sessions and metrics.
package registry

import (
	"context"
	"sync"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

// Publisher receives dashboard events. The SSE broker implements it.
type Publisher interface {
	Publish(name string, data any)
}

// Summary is the device view sent to the dashboard.
type Summary struct {
	Name                string             `json:"name"`
	State               State              `json:"state"`
	Connected           bool               `json:"connected"`
	OS                  string             `json:"os"`
	Arch                string             `json:"arch"`
	AgentVersion        string             `json:"agent_version"`
	ProtocolVersion     int                `json:"protocol_version"`
	DesiredAgentVersion string             `json:"desired_agent_version"`
	Capabilities        []string           `json:"capabilities"`
	Commands            []proto.CommandDef `json:"commands"`
	// CommandProblems are commands in the agent's config that it withheld,
	// with its reason. CommandsConfigError is why its last config reload was
	// rejected. Both are the agent's own words: display only.
	CommandProblems     []proto.CommandProblem `json:"command_problems"`
	CommandsConfigError string                 `json:"commands_config_error"`
	MAC                 string                 `json:"mac"`
	NormallyOff         bool                   `json:"normally_off"`
	TerminalEnabled     bool                   `json:"terminal_enabled"`
	Schedule            store.Schedule         `json:"schedule"`
	LastSeen            *time.Time             `json:"last_seen"`
	LastDisconnectAt    *time.Time             `json:"last_disconnect_at"`
	Incompatible        bool                   `json:"incompatible"`
	RejectReason        string                 `json:"reject_reason"`
	Metrics             *proto.Metrics         `json:"metrics"`
}

// MetricsEvent is the payload of the "metrics" SSE event.
type MetricsEvent struct {
	Device  string        `json:"device"`
	Metrics proto.Metrics `json:"metrics"`
}

// RunOutputEvent is the payload of the "run_output" SSE event: one chunk of
// a running command's output. Seq counts from 1 per run.
type RunOutputEvent struct {
	RunID   string `json:"run_id"`
	Device  string `json:"device"`
	Seq     int    `json:"seq"`
	Stream  string `json:"stream"`
	Data    string `json:"data"`
	Skipped bool   `json:"skipped"`
}

type entry struct {
	connected   bool
	lastMetrics time.Time
	ring        *Ring
	state       State
}

// Registry implements hub.Events and answers dashboard queries.
type Registry struct {
	st       *store.Store
	pub      Publisher
	interval time.Duration
	now      func() time.Time

	mu      sync.Mutex
	entries map[string]*entry
}

// New creates a registry. interval is the metrics interval.
func New(st *store.Store, pub Publisher, interval time.Duration) *Registry {
	return &Registry{st: st, pub: pub, interval: interval, now: time.Now, entries: map[string]*entry{}}
}

// Load creates entries for every stored device, all disconnected.
func (r *Registry) Load(ctx context.Context) error {
	devices, err := r.st.ListDevices(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range devices {
		r.ensureLocked(d.Name)
	}
	return nil
}

// Run re-evaluates states periodically so window edges and staleness are
// noticed without any message arriving. Blocks until ctx is done.
func (r *Registry) Run(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.reevaluateAll(ctx)
		}
	}
}

func (r *Registry) ensureLocked(name string) *entry {
	e, ok := r.entries[name]
	if !ok {
		e = &entry{ring: NewRing(60)}
		r.entries[name] = e
	}
	return e
}

// DeviceAdded registers a new device after creation via the API.
func (r *Registry) DeviceAdded(name string) {
	r.mu.Lock()
	r.ensureLocked(name)
	r.mu.Unlock()
	r.publishDevice(context.Background(), name)
}

// Refresh republishes a device after its settings changed.
func (r *Registry) Refresh(name string) {
	r.publishDevice(context.Background(), name)
}

// DeviceConnected implements hub.Events.
func (r *Registry) DeviceConnected(name string) {
	r.mu.Lock()
	e := r.ensureLocked(name)
	e.connected = true
	e.lastMetrics = r.now() // avoid a stale flicker before the first sample
	r.mu.Unlock()
	r.publishDevice(context.Background(), name)
}

// DeviceDisconnected implements hub.Events.
func (r *Registry) DeviceDisconnected(name string) {
	r.mu.Lock()
	e := r.ensureLocked(name)
	e.connected = false
	r.mu.Unlock()
	r.publishDevice(context.Background(), name)
}

// MetricsReceived implements hub.Events.
func (r *Registry) MetricsReceived(name string, m proto.Metrics) {
	r.mu.Lock()
	e := r.ensureLocked(name)
	e.lastMetrics = r.now()
	e.ring.Push(m)
	r.mu.Unlock()
	r.pub.Publish("metrics", MetricsEvent{Device: name, Metrics: m})
	r.publishDevice(context.Background(), name)
}

// RunChanged implements hub.Events.
func (r *Registry) RunChanged(runID string) {
	run, err := r.st.GetRun(context.Background(), runID)
	if err != nil {
		return
	}
	r.pub.Publish("run", run)
}

// RunOutput implements hub.Events.
func (r *Registry) RunOutput(runID, device string, seq int, o proto.CommandOutput) {
	r.pub.Publish("run_output", RunOutputEvent{
		RunID: runID, Device: device, Seq: seq, Stream: o.Stream, Data: o.Data, Skipped: o.Skipped,
	})
}

// CommandsChanged implements hub.Events.
func (r *Registry) CommandsChanged(name string) {
	r.publishDevice(context.Background(), name)
}

// UpgradeChanged implements hub.Events.
func (r *Registry) UpgradeChanged(name string) {
	r.publishDevice(context.Background(), name)
}

// Metrics returns the ring buffer for a device, oldest first.
func (r *Registry) Metrics(name string) []proto.Metrics {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[name]
	if !ok {
		return []proto.Metrics{}
	}
	return e.ring.All()
}

// Summary builds the dashboard view of one device.
func (r *Registry) Summary(ctx context.Context, name string) (*Summary, error) {
	d, err := r.st.GetDevice(ctx, name)
	if err != nil {
		return nil, err
	}
	return r.summariseDevice(ctx, d)
}

func (r *Registry) summariseDevice(ctx context.Context, d *store.Device) (*Summary, error) {
	cmds, err := r.st.ListCommands(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	problems, err := r.st.ListCommandProblems(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	return r.summarise(d, cmds, problems), nil
}

// Summaries builds the dashboard view of every device.
func (r *Registry) Summaries(ctx context.Context) ([]Summary, error) {
	devices, err := r.st.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(devices))
	for _, d := range devices {
		s, err := r.summariseDevice(ctx, d)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, nil
}

func (r *Registry) summarise(d *store.Device, cmds []proto.CommandDef, problems []proto.CommandProblem) *Summary {
	r.mu.Lock()
	e := r.ensureLocked(d.Name)
	e.state = Derive(r.now(), e.connected, e.lastMetrics, r.interval, d.NormallyOff, d.Schedule)
	s := &Summary{
		Name: d.Name, State: e.state, Connected: e.connected,
		OS: d.OS, Arch: d.Arch, AgentVersion: d.AgentVersion, ProtocolVersion: d.ProtocolVersion,
		DesiredAgentVersion: d.DesiredAgentVersion, Capabilities: d.Capabilities, Commands: cmds,
		CommandProblems: problems, CommandsConfigError: d.CommandsConfigError,
		MAC: d.MAC, NormallyOff: d.NormallyOff, TerminalEnabled: d.TerminalEnabled, Schedule: d.Schedule,
		LastSeen: d.LastSeen, LastDisconnectAt: d.LastDisconnectAt,
		Incompatible: !e.connected && d.LastRejectReason == proto.ErrProtocolVersionUnsupported,
		RejectReason: d.LastRejectReason, Metrics: e.ring.Latest(),
	}
	r.mu.Unlock()
	if s.Schedule.ExpectedOffline == nil {
		s.Schedule.ExpectedOffline = []store.Window{}
	}
	return s
}

func (r *Registry) publishDevice(ctx context.Context, name string) {
	s, err := r.Summary(ctx, name)
	if err != nil {
		return
	}
	r.pub.Publish("device", s)
}

func (r *Registry) reevaluateAll(ctx context.Context) {
	devices, err := r.st.ListDevices(ctx)
	if err != nil {
		return
	}
	for _, d := range devices {
		r.mu.Lock()
		e := r.ensureLocked(d.Name)
		prev := e.state
		next := Derive(r.now(), e.connected, e.lastMetrics, r.interval, d.NormallyOff, d.Schedule)
		r.mu.Unlock()
		if next != prev {
			r.publishDevice(ctx, d.Name)
		}
	}
}
