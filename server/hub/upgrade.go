package hub

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
)

// handleUpgradeResult records one upgrade report from an agent. Everything
// in it is untrusted: it is sanitised, matched to one of the session
// device's own upgrades, and recorded under the store's rules for which
// events may change a row.
func (h *Hub) handleUpgradeResult(ctx context.Context, s *Session, env *proto.Envelope) {
	var res proto.UpgradeResult
	if err := env.Unmarshal(&res); err != nil {
		return
	}
	// Only the server sets requested, when it creates the attempt; from an
	// agent it would move a row backwards, so it is dropped like an unknown
	// state.
	if !res.Sanitize() || res.State == proto.UpgradeRequested {
		h.opts.Log.Warn("upgrade result dropped: bad state", "device", s.DeviceName, "state", res.State)
		return
	}
	st := h.opts.Store
	u, err := st.FindUpgradeForResult(ctx, s.DeviceID, res.UpgradeID, res.ToVersion)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			h.opts.Log.Error("find upgrade for result", "device", s.DeviceName, "err", err)
			return
		}
		h.opts.Log.Warn("upgrade result matches no upgrade", "device", s.DeviceName,
			"upgrade_id", res.UpgradeID, "to", res.ToVersion, "state", res.State)
		st.Audit(ctx, "agent:"+s.DeviceName, "upgrade_result_unmatched", s.DeviceName, res.State, res.UpgradeID)
		return
	}
	applied, err := st.RecordUpgradeEvent(ctx, store.UpgradeEvent{
		UpgradeID: u.ID, Source: store.UpgradeSourceAgent,
		State: res.State, Reason: res.Reason, Detail: res.Detail,
	})
	if err != nil {
		h.opts.Log.Error("record upgrade event", "device", s.DeviceName, "upgrade_id", u.ID, "state", res.State, "err", err)
		return
	}
	// The audit row is built from the server's own row plus the validated
	// reason code. The agent's detail text never goes in the audit log.
	detail := u.ID + " " + u.FromVersion + " -> " + u.ToVersion
	if res.Reason != "" {
		detail += " " + res.Reason
	}
	if !applied {
		detail += " (not applied)"
	}
	st.Audit(ctx, "agent:"+s.DeviceName, "upgrade_state", s.DeviceName, res.State, detail)
	h.opts.Events.UpgradeChanged(s.DeviceName)
	if applied && !upgradeInFlight(res.State) {
		h.RefreshBlocked(ctx)
	}
}

func upgradeInFlight(state string) bool {
	u := store.Upgrade{State: state}
	return u.IsInFlight()
}

// checkUpgradeAfterHandshake infers what it can about the device's newest
// upgrade from the version the agent connected at, then offers the device
// its desired version. The agent's own report, if one follows, supersedes
// an inferred outcome.
func (h *Hub) checkUpgradeAfterHandshake(ctx context.Context, s *Session, agentVersion string) {
	st := h.opts.Store
	d, err := st.GetDeviceByID(ctx, s.DeviceID)
	if err != nil {
		return
	}
	u, err := st.GetLatestUpgrade(ctx, d.ID)
	if err != nil {
		h.opts.Log.Error("get latest upgrade", "device", d.Name, "err", err)
	}
	if u != nil && u.IsInFlight() {
		switch {
		case agentVersion == u.ToVersion:
			// Covers a lost verified report and a swap that left no marker.
			h.inferUpgradeOutcome(ctx, d, u, proto.UpgradeVerified, store.UpgradeReasonHandshakeAtToVersion, "upgrade_verify_detected")
		case agentVersion == u.FromVersion && store.UpgradePastSwap(u.State):
			h.inferUpgradeOutcome(ctx, d, u, proto.UpgradeRolledBack, store.UpgradeReasonHandshakeAtFromVersion, "upgrade_rollback_detected")
		}
		// Before the swap a handshake at from_version may be a plain
		// reconnect; SweepUpgrades times the row out if the agent has gone
		// quiet.
	}
	id, err := h.evaluateUpgrade(ctx, d, s, false)
	var inFlight *InFlightError
	if errors.As(err, &inFlight) && inFlight.Device == d.Name {
		// Its own upgrade is still running; what was recorded when that
		// was sent still stands.
		return
	}
	h.recordDispatch(ctx, d, id, err)
}

func (h *Hub) inferUpgradeOutcome(ctx context.Context, d *store.Device, u *store.Upgrade, state, reason, auditAction string) {
	st := h.opts.Store
	applied, err := st.RecordUpgradeEvent(ctx, store.UpgradeEvent{
		UpgradeID: u.ID, Source: store.UpgradeSourceServer, State: state, Reason: reason,
		Detail: "agent connected at version " + d.AgentVersion,
	})
	if err != nil {
		h.opts.Log.Error("record inferred upgrade outcome", "device", d.Name, "upgrade_id", u.ID, "err", err)
		return
	}
	if !applied {
		return
	}
	st.Audit(ctx, "server", auditAction, d.Name, state, u.ID)
	h.opts.Events.UpgradeChanged(d.Name)
	h.RefreshBlocked(ctx)
}

// noteHandshakeRejected adds a handshake_rejected line to the timeline of a
// device's upgrade when the new binary is refused for its protocol version.
// It is the one rollback cause the server sees for itself. The caller has
// verified the device token; hello is otherwise untrusted.
func (h *Hub) noteHandshakeRejected(ctx context.Context, d *store.Device, hello *proto.Hello) {
	st := h.opts.Store
	u, err := st.GetLatestUpgrade(ctx, d.ID)
	if err != nil || u == nil || !store.UpgradePastSwap(u.State) {
		return
	}
	version := hello.AgentVersion
	if len(version) > 64 {
		version = version[:64]
	}
	// Sanitize bounds the text and strips control characters.
	note := proto.UpgradeResult{State: u.State, Detail: fmt.Sprintf("agent %s speaks protocol %d; server accepts %d-%d",
		version, hello.ProtocolVersion, proto.MinSupported, proto.Version)}
	note.Sanitize()
	ev := store.UpgradeEvent{
		UpgradeID: u.ID, Source: store.UpgradeSourceServer, State: u.State,
		Reason: store.UpgradeReasonHandshakeRejected, Detail: note.Detail,
	}
	// The agent retries its handshake every few seconds; one line is enough.
	events, err := st.ListUpgradeEvents(ctx, u.ID)
	if err != nil {
		return
	}
	if n := len(events); n > 0 {
		last := events[n-1]
		if last.Source == ev.Source && last.State == ev.State && last.Reason == ev.Reason && last.Detail == ev.Detail {
			return
		}
	}
	if _, err := st.RecordUpgradeEvent(ctx, ev); err != nil {
		h.opts.Log.Warn("record handshake_rejected", "device", d.Name, "upgrade_id", u.ID, "err", err)
		return
	}
	h.opts.Events.UpgradeChanged(d.Name)
}

// Reasons evaluateUpgrade may decline to send an upgrade request.
var (
	ErrUpgradeNotNeeded    = errors.New("no desired version set or device already at it")
	ErrUpgradeInFlight     = errors.New("another upgrade is already in flight")
	ErrUpgradePrevFailed   = errors.New("a previous upgrade to this version failed")
	ErrUpgradeNoRelease    = errors.New("no release ingested for target version/os/arch")
	ErrUpgradeNotConnected = errors.New("device is not connected")
)

// InFlightError is ErrUpgradeInFlight with the upgrade that holds the
// fleet's one slot, so a message can name the holder.
// errors.Is(err, ErrUpgradeInFlight) is true for it.
type InFlightError struct {
	Device    string // device holding the slot
	UpgradeID string
	State     string
	Since     time.Time // when State began

	FromVersion string
	ToVersion   string

	age time.Duration // time in State when the error was made
}

func (e *InFlightError) Error() string {
	return fmt.Sprintf("another upgrade is in flight: %s %s -> %s, %s for %s",
		e.Device, e.FromVersion, e.ToVersion, e.State, shortDuration(e.age))
}

func (e *InFlightError) Unwrap() error { return ErrUpgradeInFlight }

// shortDuration formats d coarsely for a one-line message: "45s", "12m",
// "3h5m".
func shortDuration(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%dh%dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

func upgradeNeeded(d *store.Device) bool {
	return d.DesiredAgentVersion != "" && d.DesiredAgentVersion != d.AgentVersion
}

// evaluateUpgrade sends d an upgrade request for its desired agent version.
// The error is nil only if the request was sent; upgradeID is the row
// created for it, and is also set when the row was created but the send
// failed. When explicit is false (automatic offers on handshake or on a
// desired-version change), a prior failed or rolled_back attempt at the same
// target version blocks the offer so a bad release is not retried in a loop.
// An explicit operator retry skips only that check; the
// one-active-upgrade-in-fleet guard always applies.
func (h *Hub) evaluateUpgrade(ctx context.Context, d *store.Device, s *Session, explicit bool) (upgradeID string, err error) {
	if !upgradeNeeded(d) {
		return "", ErrUpgradeNotNeeded
	}
	id, rel, err := h.reserveUpgrade(ctx, d, explicit)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("/api/agent/releases/%s/%s_%s", rel.Version, rel.OS, rel.Arch)
	req := proto.UpgradeRequest{
		Version: rel.Version, OS: rel.OS, Arch: rel.Arch,
		SHA256: rel.SHA256, SizeBytes: rel.SizeBytes, Signature: rel.Signature,
		URL: url, UpgradeID: id,
	}
	env, _ := proto.New(proto.TypeUpgradeRequest, req)
	if err := s.Send(env); err != nil {
		h.opts.Log.Error("send upgrade request", "device", d.Name, "err", err)
		// Close the row so it does not hold the fleet-wide in-flight slot.
		if _, rerr := h.opts.Store.RecordUpgradeEvent(ctx, store.UpgradeEvent{
			UpgradeID: id, Source: store.UpgradeSourceServer, State: proto.UpgradeFailed,
			Reason: store.UpgradeReasonSendFailed, Detail: err.Error(),
		}); rerr != nil {
			h.opts.Log.Error("close unsent upgrade", "device", d.Name, "upgrade_id", id, "err", rerr)
		}
		return id, fmt.Errorf("send upgrade request: %w", err)
	}
	h.opts.Store.Audit(ctx, "server", "upgrade_request", d.Name, "sent", id)
	h.opts.Log.Info("upgrade request sent", "device", d.Name, "to", d.DesiredAgentVersion)
	return id, nil
}

// reserveUpgrade runs the checks that may decline an upgrade and, if none
// does, creates its row. It holds upgradeMu throughout, so of several
// concurrent evaluations only one finds the fleet slot free.
func (h *Hub) reserveUpgrade(ctx context.Context, d *store.Device, explicit bool) (id string, rel *store.Release, err error) {
	h.upgradeMu.Lock()
	defer h.upgradeMu.Unlock()
	active, err := h.opts.Store.GetActiveUpgrade(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("check active upgrade: %w", err)
	}
	if active != nil {
		return "", nil, &InFlightError{
			Device: active.DeviceName, UpgradeID: active.ID, State: active.State, Since: active.UpdatedAt,
			FromVersion: active.FromVersion, ToVersion: active.ToVersion,
			age: h.opts.now().Sub(active.UpdatedAt),
		}
	}
	if !explicit && h.opts.Store.HasFailedUpgrade(ctx, d.ID, d.DesiredAgentVersion) {
		return "", nil, ErrUpgradePrevFailed
	}
	rel, err = h.opts.Store.GetRelease(ctx, d.DesiredAgentVersion, d.OS, d.Arch)
	if err != nil {
		h.opts.Log.Warn("no release for target version", "device", d.Name,
			"version", d.DesiredAgentVersion, "os", d.OS, "arch", d.Arch)
		if errors.Is(err, store.ErrNotFound) {
			return "", nil, ErrUpgradeNoRelease
		}
		return "", nil, fmt.Errorf("get release: %w", err)
	}
	requestedBy := "server"
	if explicit {
		requestedBy = "admin"
	}
	id, err = h.opts.Store.CreateUpgrade(ctx, d.ID, d.AgentVersion, d.DesiredAgentVersion, requestedBy)
	if err != nil {
		h.opts.Log.Error("create upgrade row", "err", err)
		return "", nil, fmt.Errorf("create upgrade row: %w", err)
	}
	return id, rel, nil
}

// SendUpgradeToDevice triggers an automatic upgrade evaluation for a device
// and records the outcome on it. The error is nil only if an upgrade request
// was sent, and upgradeID is then the new upgrade's id.
func (h *Hub) SendUpgradeToDevice(ctx context.Context, d *store.Device) (upgradeID string, err error) {
	return h.sendUpgrade(ctx, d, false)
}

// RetryUpgrade is SendUpgradeToDevice for an explicit operator retry: it is
// not blocked by an earlier failed or rolled_back attempt at the same version.
func (h *Hub) RetryUpgrade(ctx context.Context, d *store.Device) (upgradeID string, err error) {
	return h.sendUpgrade(ctx, d, true)
}

func (h *Hub) sendUpgrade(ctx context.Context, d *store.Device, explicit bool) (string, error) {
	var id string
	var err error
	// Checked before the session so that clearing the target of an offline
	// device clears its dispatch status instead of reading not_connected.
	if !upgradeNeeded(d) {
		err = ErrUpgradeNotNeeded
	} else if s := h.Session(d.Name); s == nil {
		err = ErrUpgradeNotConnected
	} else {
		id, err = h.evaluateUpgrade(ctx, d, s, explicit)
	}
	var inFlight *InFlightError
	if errors.As(err, &inFlight) && inFlight.Device == d.Name && inFlight.ToVersion == d.DesiredAgentVersion {
		// The handshake offer got there first: this device's upgrade to the
		// same target is already running and what was recorded for it
		// stands. A different target is a real refusal and is recorded.
		return id, err
	}
	h.recordDispatch(ctx, d, id, err)
	return id, err
}

// DispatchCode maps the error of an upgrade evaluation to its dispatch
// code. ErrUpgradeNotNeeded has none and maps to "".
func DispatchCode(err error) string {
	switch {
	case err == nil:
		return store.DispatchSent
	case errors.Is(err, ErrUpgradeNotNeeded):
		return ""
	case errors.Is(err, ErrUpgradeInFlight):
		return store.DispatchInFlight
	case errors.Is(err, ErrUpgradePrevFailed):
		return store.DispatchPrevFailed
	case errors.Is(err, ErrUpgradeNoRelease):
		return store.DispatchNoRelease
	case errors.Is(err, ErrUpgradeNotConnected):
		return store.DispatchNotConnected
	}
	return store.DispatchSendFailed
}

// recordDispatch stores the outcome of an upgrade evaluation on the device
// and publishes UpgradeChanged, so the dashboard can say why nothing was
// sent. err is what evaluateUpgrade returned.
func (h *Hub) recordDispatch(ctx context.Context, d *store.Device, upgradeID string, err error) {
	disp := store.UpgradeDispatch{Code: DispatchCode(err), UpgradeID: upgradeID, At: h.opts.now().UTC()}
	switch disp.Code {
	case "":
		if d.UpgradeDispatch == nil {
			return // nothing stored, nothing to clear
		}
		disp = store.UpgradeDispatch{}
	case store.DispatchSent:
		disp.Message = "upgrade request sent"
	default:
		disp.Message = err.Error()
		var inFlight *InFlightError
		if errors.As(err, &inFlight) {
			since := inFlight.Since.UTC()
			disp.BlockingDevice = inFlight.Device
			disp.BlockingState = inFlight.State
			disp.BlockingSince = &since
		}
	}
	if err := h.opts.Store.SetUpgradeDispatch(ctx, d.ID, disp); err != nil {
		h.opts.Log.Error("record upgrade dispatch", "device", d.Name, "err", err)
		return
	}
	h.opts.Events.UpgradeChanged(d.Name)
}

// RefreshBlocked is called when an upgrade reaches a terminal state. Every
// device that was told another upgrade was in flight is moved to waiting.
// Nothing is re-offered: the operator presses Upgrade now, or the device's
// next handshake offers it.
func (h *Hub) RefreshBlocked(ctx context.Context) {
	st := h.opts.Store
	devices, err := st.ListDevices(ctx)
	if err != nil {
		h.opts.Log.Error("refresh blocked upgrades", "err", err)
		return
	}
	for _, d := range devices {
		if d.UpgradeDispatch == nil || d.UpgradeDispatch.Code != store.DispatchInFlight {
			continue
		}
		disp := store.UpgradeDispatch{
			Code:    store.DispatchWaiting,
			Message: "the upgrade that was blocking this one has ended",
			At:      h.opts.now().UTC(),
		}
		if err := st.SetUpgradeDispatch(ctx, d.ID, disp); err != nil {
			h.opts.Log.Error("refresh blocked upgrades", "device", d.Name, "err", err)
			continue
		}
		h.opts.Events.UpgradeChanged(d.Name)
	}
}

// SweepUpgrades deals with an in-flight upgrade that has stopped reporting.
// now is the caller's clock. Before the swap the device's binary is
// untouched, so the upgrade is closed as failed/timed_out and the fleet slot
// freed; a late result from a still-working agent supersedes that. After the
// swap the upgrade keeps the slot, because a release that may just have
// broken one device must not be offered to the next: it is only reported as
// stalled, and UpgradeChanged is published for its device on every sweep so
// open pages pick the flag up.
func (h *Hub) SweepUpgrades(ctx context.Context, now time.Time) {
	st := h.opts.Store
	// There is normally at most one in-flight row. The bound only guards
	// against spinning should there ever be more.
	for range 8 {
		u, err := st.GetActiveUpgrade(ctx)
		if err != nil {
			h.opts.Log.Error("sweep upgrades", "err", err)
			return
		}
		if u == nil {
			return
		}
		limit, ok := store.UpgradeStaleAfter(u.State)
		if !ok {
			// A state this build does not know, left by an older server
			// that stored whatever the agent sent. It must not hold the
			// slot for good.
			limit = 2 * time.Minute
		}
		age := now.Sub(u.UpdatedAt)
		if age <= limit {
			return
		}
		if store.UpgradePastSwap(u.State) {
			h.opts.Events.UpgradeChanged(u.DeviceName)
			return
		}
		if _, err := st.RecordUpgradeEvent(ctx, store.UpgradeEvent{
			UpgradeID: u.ID, TS: now, Source: store.UpgradeSourceServer,
			State: proto.UpgradeFailed, Reason: store.UpgradeReasonTimedOut,
			Detail: fmt.Sprintf("no report from the agent for %s while %s", shortDuration(age), u.State),
		}); err != nil {
			h.opts.Log.Error("time out upgrade", "device", u.DeviceName, "upgrade_id", u.ID, "err", err)
			return
		}
		h.opts.Log.Warn("upgrade timed out", "device", u.DeviceName, "upgrade_id", u.ID, "state", u.State)
		st.Audit(ctx, "server", "upgrade_timeout", u.DeviceName, proto.UpgradeFailed, u.ID+" "+u.State)
		h.opts.Events.UpgradeChanged(u.DeviceName)
		h.RefreshBlocked(ctx)
	}
}
