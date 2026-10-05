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
)

const outputCap = 64 << 10

type inflight struct {
	runID      string
	sessionID  string
	deviceID   int64
	deviceName string
	command    string
	dispatched bool
	timer      *time.Timer
	stdout     strings.Builder
	stderr     strings.Builder
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

func (r *router) markDispatched(f *inflight) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f.dispatched = true
}

func (r *router) appendOutput(f *inflight, stream, data string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := &f.stdout
	if stream == "stderr" {
		b = &f.stderr
	}
	if room := outputCap - b.Len(); room > 0 {
		if len(data) > room {
			data = data[:room]
		}
		b.WriteString(data)
	}
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
	h.opts.Store.FinishRun(ctx, f.runID, status, nil, f.stdout.String(), f.stderr.String(), false)
	h.opts.Store.Audit(ctx, "server", "command_result", f.deviceName+"/"+f.command, status, f.runID)
	h.opts.Events.RunChanged(f.runID)
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
	stdout, stderr := res.Stdout, res.Stderr
	if stdout == "" && stderr == "" {
		stdout, stderr = f.stdout.String(), f.stderr.String()
	}
	h.opts.Store.FinishRun(ctx, f.runID, res.Status, exit, stdout, stderr, res.Truncated)
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
	h.router.appendOutput(f, o.Stream, o.Data)
}

// sessionEnded finalises every run the session had in flight.
func (h *Hub) sessionEnded(ctx context.Context, s *Session) {
	for _, f := range h.router.removeSession(s.ID) {
		f.timer.Stop()
		status := proto.RunLost
		if f.dispatched {
			status = proto.RunDisconnectedAsExpected
		}
		h.opts.Store.FinishRun(ctx, f.runID, status, nil, f.stdout.String(), f.stderr.String(), false)
		h.opts.Store.Audit(ctx, "server", "command_result", f.deviceName+"/"+f.command, status, f.runID)
		h.opts.Events.RunChanged(f.runID)
	}
	h.opts.Store.MarkDeviceRunsLost(ctx, s.DeviceID)
}
