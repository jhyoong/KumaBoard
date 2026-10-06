package hub

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

var (
	ErrNotConnected   = errors.New("hub: device not connected")
	ErrUnknownCommand = errors.New("hub: command not declared by device")
	// ErrRunNotInFlight: the run already ended, was dispatched (the device
	// is expected to drop off), or is not known to this server process.
	ErrRunNotInFlight = errors.New("hub: run is not in flight")
	// ErrCancelUnsupported: the agent speaks protocol 1, which has no cancel.
	ErrCancelUnsupported = errors.New("hub: agent does not support cancel")
)

// outputCap is how much of each stream the server keeps for a run: the last
// 64 KiB. The agent applies the same bound, but the agent is untrusted, so
// it is enforced again here on every chunk and on the final result.
const outputCap = proto.MaxCommandOutput

// tailBuf keeps the newest outputCap bytes appended to it.
type tailBuf struct {
	s       string
	dropped bool // something was cut from the front
}

// add appends data and returns what of it was kept: a chunk larger than the
// cap is cut to its own tail first.
func (b *tailBuf) add(data string) (kept string, cut bool) {
	kept = proto.TailText(data, outputCap)
	cut = len(kept) < len(data)
	joined := b.s + kept
	b.s = proto.TailText(joined, outputCap)
	if cut || len(b.s) < len(joined) {
		b.dropped = true
	}
	return kept, cut
}

type inflight struct {
	runID      string
	sessionID  string
	deviceID   int64
	deviceName string
	command    string
	dispatched bool
	timer      *time.Timer
	stdout     tailBuf
	stderr     tailBuf
	seq        int // run_output events published so far
}

// output returns the buffered tails. Call with router.mu held, or after the
// run was removed from the router.
func (f *inflight) output() (stdout, stderr string, truncated bool) {
	return f.stdout.s, f.stderr.s, f.stdout.dropped || f.stderr.dropped
}

type router struct {
	mu   sync.Mutex
	runs map[string]*inflight
}

func newRouter() *router { return &router{runs: map[string]*inflight{}} }

func (r *router) add(f *inflight) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs[f.runID] = f
}

func (r *router) get(id string) *inflight {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs[id]
}

func (r *router) remove(f *inflight) (dispatched bool, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, present := r.runs[f.runID]; !present {
		return false, false
	}
	delete(r.runs, f.runID)
	return f.dispatched, true
}

func (r *router) isDispatched(f *inflight) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return f.dispatched
}

func (r *router) markDispatched(f *inflight) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f.dispatched = true
}

// appendOutput adds a chunk to the run's tail buffer and returns what to
// publish for it: the chunk as kept, and its server-assigned seq. ok is false
// if the run is gone or the stream unknown.
func (r *router) appendOutput(f *inflight, o proto.CommandOutput) (out proto.CommandOutput, seq int, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, present := r.runs[f.runID]; !present {
		return out, 0, false
	}
	var b *tailBuf
	switch o.Stream {
	case proto.StreamStdout:
		b = &f.stdout
	case proto.StreamStderr:
		b = &f.stderr
	default:
		return out, 0, false
	}
	if o.Skipped {
		b.dropped = true
	}
	kept, cut := b.add(o.Data)
	f.seq++
	return proto.CommandOutput{Seq: f.seq, Stream: o.Stream, Data: kept, Skipped: o.Skipped || cut}, f.seq, true
}

// snapshot returns the live tails of a run still in flight.
func (r *router) snapshot(runID string) (stdout, stderr string, truncated bool, seq int, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.runs[runID]
	if f == nil {
		return "", "", false, 0, false
	}
	stdout, stderr, truncated = f.output()
	return stdout, stderr, truncated, f.seq, true
}

func (r *router) removeSession(sessionID string) []*inflight {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*inflight
	for id, f := range r.runs {
		if f.sessionID == sessionID {
			out = append(out, f)
			delete(r.runs, id)
		}
	}
	return out
}

// RequestCommand dispatches a named command to a connected device and returns the run ID.
func (h *Hub) RequestCommand(ctx context.Context, deviceName, command, requestedBy string) (string, error) {
	s := h.Session(deviceName)
	if s == nil {
		return "", ErrNotConnected
	}
	cmds, err := h.opts.Store.ListCommands(ctx, s.DeviceID)
	if err != nil {
		return "", err
	}
	var def *proto.CommandDef
	for i := range cmds {
		if cmds[i].Name == command {
			def = &cmds[i]
		}
	}
	if def == nil {
		return "", ErrUnknownCommand
	}
	env, err := proto.New(proto.TypeCommandRequest, proto.CommandRequest{Name: command})
	if err != nil {
		return "", err
	}
	run := &store.Run{ID: env.ID, DeviceID: s.DeviceID, Command: command, RequestedBy: requestedBy,
		RequestedAt: time.Now(), Status: proto.RunRunning}
	if err := h.opts.Store.InsertRun(ctx, run); err != nil {
		return "", err
	}
	f := &inflight{runID: run.ID, sessionID: s.ID, deviceID: s.DeviceID, deviceName: deviceName, command: command}
	f.timer = time.AfterFunc(time.Duration(def.TimeoutS+5)*time.Second, func() { h.runTimedOut(f) })
	h.router.add(f)
	if err := s.Send(env); err != nil {
		h.router.remove(f)
		f.timer.Stop()
		h.opts.Store.FinishRun(ctx, run.ID, proto.RunLost, nil, "", "", false)
		h.opts.Events.RunChanged(run.ID)
		return run.ID, err
	}
	h.opts.Store.Audit(ctx, requestedBy, "command_request", deviceName+"/"+command, "sent", run.ID)
	h.opts.Events.RunChanged(run.ID)
	return run.ID, nil
}

func (h *Hub) runTimedOut(f *inflight) {
	dispatched, ok := h.router.remove(f)
	if !ok {
		return
	}
	f.timer.Stop()
	status := proto.RunTimeout
	if dispatched {
		status = proto.RunNoDisconnect
	}
	ctx := context.Background()
	stdout, stderr, truncated := f.output()
	h.opts.Store.FinishRun(ctx, f.runID, status, nil, stdout, stderr, truncated)
	h.opts.Store.Audit(ctx, "server", "command_result", f.deviceName+"/"+f.command, status, f.runID)
	h.opts.Events.RunChanged(f.runID)
}

// CancelRun asks the agent to stop a run in flight. The run stays running
// until the agent's command_result arrives; the timeout timer remains the
// backstop. Only the run ID is sent.
func (h *Hub) CancelRun(ctx context.Context, runID, requestedBy string) error {
	f := h.router.get(runID)
	if f == nil || h.router.isDispatched(f) {
		return ErrRunNotInFlight
	}
	s := h.Session(f.deviceName)
	if s == nil || s.ID != f.sessionID {
		return ErrRunNotInFlight
	}
	if s.ProtocolVersion < 2 {
		return ErrCancelUnsupported
	}
	env, err := proto.New(proto.TypeCommandCancel, nil)
	if err != nil {
		return err
	}
	env.ID = runID
	if err := s.Send(env); err != nil {
		return err
	}
	h.opts.Store.Audit(ctx, requestedBy, "command_cancel", f.deviceName+"/"+f.command, "sent", runID)
	return nil
}

// LiveOutput returns the output buffered so far for a run still in flight,
// and the seq of the last run_output event it includes.
func (h *Hub) LiveOutput(runID string) (stdout, stderr string, truncated bool, seq int, ok bool) {
	return h.router.snapshot(runID)
}

func (h *Hub) handleCommandResult(ctx context.Context, s *Session, env *proto.Envelope) {
	f := h.router.get(env.ID)
	if f == nil || f.sessionID != s.ID {
		h.opts.Log.Warn("result for unknown run", "device", s.DeviceName, "id", env.ID)
		return
	}
	var res proto.CommandResult
	if err := env.Unmarshal(&res); err != nil {
		return
	}
	if res.Status == proto.RunDispatched {
		h.router.markDispatched(f)
		h.opts.Store.SetRunStatus(ctx, f.runID, proto.RunDispatched)
		h.opts.Events.RunChanged(f.runID)
		return
	}
	if _, ok := h.router.remove(f); !ok {
		return
	}
	f.timer.Stop()
	var exit *int
	if res.Status == proto.RunOK || res.Status == proto.RunFailed {
		v := res.ExitCode
		exit = &v
	}
	// The result's output is authoritative; streamed chunks only stand in
	// when it carries none. FinishRun cuts both to the last 64 KiB.
	stdout, stderr, truncated := res.Stdout, res.Stderr, res.Truncated
	if stdout == "" && stderr == "" {
		stdout, stderr, truncated = f.output()
		truncated = truncated || res.Truncated
	}
	h.opts.Store.FinishRun(ctx, f.runID, res.Status, exit, stdout, stderr, truncated)
	h.opts.Store.Audit(ctx, "agent:"+s.DeviceName, "command_result", s.DeviceName+"/"+f.command, res.Status, f.runID)
	h.opts.Events.RunChanged(f.runID)
}

func (h *Hub) handleCommandOutput(s *Session, env *proto.Envelope) {
	f := h.router.get(env.ID)
	if f == nil || f.sessionID != s.ID {
		return
	}
	var o proto.CommandOutput
	if err := env.Unmarshal(&o); err != nil {
		return
	}
	if out, seq, ok := h.router.appendOutput(f, o); ok {
		h.opts.Events.RunOutput(f.runID, f.deviceName, seq, out)
	}
}

// handleCommandsUpdate replaces the device's declared commands after the
// agent re-read its own config. The server never asks for this.
func (h *Hub) handleCommandsUpdate(ctx context.Context, s *Session, env *proto.Envelope) {
	var u proto.CommandsUpdate
	if err := env.Unmarshal(&u); err != nil {
		return
	}
	ch, err := h.opts.Store.ReplaceCommands(ctx, s.DeviceID, proto.SanitizeCommands(u.Commands),
		proto.SanitizeProblems(u.Problems), proto.SanitizeConfigError(u.ConfigError))
	if err != nil {
		h.opts.Log.Error("replace commands", "device", s.DeviceName, "err", err)
		return
	}
	if !ch.Changed {
		return
	}
	detail := "added: " + strings.Join(ch.Added, ",") + "; removed: " + strings.Join(ch.Removed, ",")
	h.opts.Store.Audit(ctx, "agent:"+s.DeviceName, "commands_update", s.DeviceName, "ok", detail)
	h.opts.Events.CommandsChanged(s.DeviceName)
}

// sessionEnded finalises every run the session had in flight.
func (h *Hub) sessionEnded(ctx context.Context, s *Session) {
	for _, f := range h.router.removeSession(s.ID) {
		f.timer.Stop()
		status := proto.RunLost
		if f.dispatched {
			status = proto.RunDisconnectedAsExpected
		}
		stdout, stderr, truncated := f.output()
		h.opts.Store.FinishRun(ctx, f.runID, status, nil, stdout, stderr, truncated)
		h.opts.Store.Audit(ctx, "server", "command_result", f.deviceName+"/"+f.command, status, f.runID)
		h.opts.Events.RunChanged(f.runID)
	}
	h.opts.Store.MarkDeviceRunsLost(ctx, s.DeviceID)
}
