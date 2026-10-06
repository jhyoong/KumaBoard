package api

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/store"
	"github.com/jhyoong/KumaBoard/server/terminal"
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
var macRe = regexp.MustCompile(`^([0-9A-Fa-f]{2}[:-]){5}[0-9A-Fa-f]{2}$`)

type deviceBody struct {
	Name                string         `json:"name"`
	MAC                 string         `json:"mac"`
	NormallyOff         bool           `json:"normally_off"`
	TerminalEnabled     bool           `json:"terminal_enabled"`
	Schedule            store.Schedule `json:"schedule"`
	DesiredAgentVersion *string        `json:"desired_agent_version,omitempty"`
}

func (b *deviceBody) validate(requireName bool) error {
	if requireName && !nameRe.MatchString(b.Name) {
		return errors.New("name must be lowercase letters, digits, and hyphens")
	}
	if b.MAC != "" && !macRe.MatchString(b.MAC) {
		return errors.New("mac must look like aa:bb:cc:dd:ee:ff")
	}
	if b.Schedule.GracePeriodS < 0 {
		return errors.New("grace_period_s must not be negative")
	}
	for _, w := range b.Schedule.ExpectedOffline {
		if len(w.From) != 5 || len(w.To) != 5 {
			return errors.New("window times must be HH:MM")
		}
	}
	return nil
}

func (s *server) listDevices(w http.ResponseWriter, r *http.Request) {
	sums, err := s.Registry.Summaries(r.Context())
	if err != nil {
		s.Log.Error("list devices", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, sums)
}

func (s *server) registerDevice(w http.ResponseWriter, r *http.Request) {
	var body deviceBody
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}
	if err := body.validate(true); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	d, token, err := s.Store.CreateDevice(r.Context(), body.Name, body.MAC, body.NormallyOff, body.Schedule)
	if errors.Is(err, store.ErrExists) {
		writeError(w, http.StatusConflict, "device already exists")
		return
	}
	if err != nil {
		s.Log.Error("register device", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.Registry.DeviceAdded(d.Name)
	s.Store.Audit(r.Context(), s.actor(r), "device_register", d.Name, "ok", "")
	sum, _ := s.Registry.Summary(r.Context(), d.Name)
	writeJSON(w, http.StatusCreated, map[string]any{"device": sum, "token": token})
}

// devicePatchBody is the PATCH body for updateDevice. Every field is a
// pointer so an omitted field means "keep the stored value". A value-typed
// body here silently wiped mac/normally_off when a client sent a partial
// PATCH (e.g. the upgrade panel sending only desired_agent_version), which
// destroyed WOL settings.
type devicePatchBody struct {
	MAC                 *string         `json:"mac"`
	NormallyOff         *bool           `json:"normally_off"`
	TerminalEnabled     *bool           `json:"terminal_enabled"`
	Schedule            *store.Schedule `json:"schedule"`
	DesiredAgentVersion *string         `json:"desired_agent_version,omitempty"`
}

func (b *devicePatchBody) validate() error {
	if b.MAC != nil && !macRe.MatchString(*b.MAC) {
		return errors.New("mac must look like aa:bb:cc:dd:ee:ff")
	}
	if b.Schedule != nil {
		if b.Schedule.GracePeriodS < 0 {
			return errors.New("grace_period_s must not be negative")
		}
		for _, w := range b.Schedule.ExpectedOffline {
			if len(w.From) != 5 || len(w.To) != 5 {
				return errors.New("window times must be HH:MM")
			}
		}
	}
	return nil
}

// scheduleEqual compares two schedules field by field so the audit diff
// reports "schedule" only when it really changed.
func scheduleEqual(a, b store.Schedule) bool {
	if a.GracePeriodS != b.GracePeriodS || len(a.ExpectedOffline) != len(b.ExpectedOffline) {
		return false
	}
	for i := range a.ExpectedOffline {
		if a.ExpectedOffline[i] != b.ExpectedOffline[i] {
			return false
		}
	}
	return true
}

func (s *server) updateDevice(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body devicePatchBody
	if err := readJSON(w, r, &body); err != nil {
		// An empty body is a valid PATCH: nothing to change.
		if !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "bad request")
			return
		}
	}
	if err := body.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	prev, err := s.Store.GetDevice(r.Context(), name)
	if err != nil {
		s.notFoundOr500(w, err)
		return
	}
	// Merge the partial PATCH onto the stored settings and remember which
	// fields actually change so the audit row records the diff.
	var changed []string
	mac, normallyOff, terminalEnabled := prev.MAC, prev.NormallyOff, prev.TerminalEnabled
	sched := prev.Schedule
	if body.MAC != nil && *body.MAC != prev.MAC {
		mac = *body.MAC
		changed = append(changed, "mac")
	}
	if body.NormallyOff != nil && *body.NormallyOff != prev.NormallyOff {
		normallyOff = *body.NormallyOff
		changed = append(changed, "normally_off")
	}
	if body.TerminalEnabled != nil && *body.TerminalEnabled != prev.TerminalEnabled {
		terminalEnabled = *body.TerminalEnabled
		changed = append(changed, "terminal_enabled")
	}
	if body.Schedule != nil && !scheduleEqual(*body.Schedule, prev.Schedule) {
		sched = *body.Schedule
		changed = append(changed, "schedule")
	}
	if err := s.Store.UpdateDeviceSettings(r.Context(), name, mac, normallyOff, sched, terminalEnabled); err != nil {
		s.notFoundOr500(w, err)
		return
	}
	s.Registry.Refresh(name)
	actor := s.actor(r)
	s.Store.Audit(r.Context(), actor, "device_update", name, "ok", strings.Join(changed, ","))
	// Turning the web shell on or off gets its own row so it stands out from
	// routine settings saves, and only fires when the merged value actually
	// flips.
	if prev.TerminalEnabled != terminalEnabled {
		action := "terminal_disable"
		if terminalEnabled {
			action = "terminal_enable"
		}
		s.Store.Audit(r.Context(), actor, action, name, "ok", "")
		// Disabling the shell takes effect now: end live sessions and void
		// tickets already issued. requestTerminal re-checks the flag after
		// issuing a ticket, so one issued concurrently with this update is
		// voided too.
		if !terminalEnabled && s.Terminal != nil {
			s.Terminal.CloseDevice(name, terminal.ReasonTerminalDisabled)
		}
	}
	if body.DesiredAgentVersion != nil {
		if err := s.Store.SetDesiredAgentVersion(r.Context(), name, *body.DesiredAgentVersion); err != nil {
			s.notFoundOr500(w, err)
			return
		}
		s.Store.Audit(r.Context(), actor, "set_desired_version", name, "ok", *body.DesiredAgentVersion)
		// The hub records what came of the offer on the device, so the
		// summary returned below says whether a request went out and, if
		// not, why.
		if d, _ := s.Store.GetDevice(r.Context(), name); d != nil {
			s.tryUpgrade(r.Context(), d)
		}
	}
	sum, _ := s.Registry.Summary(r.Context(), name)
	writeJSON(w, http.StatusOK, sum)
}

func (s *server) revokeDevice(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.Store.RevokeToken(r.Context(), name); err != nil {
		s.notFoundOr500(w, err)
		return
	}
	s.Hub.CloseDevice(name, "token revoked")
	// Terminal sockets are ticket-authenticated and outlive the control
	// connection, so end them explicitly.
	if s.Terminal != nil {
		s.Terminal.CloseDevice(name, terminal.ReasonDeviceRevoked)
	}
	s.Store.Audit(r.Context(), s.actor(r), "token_revoke", name, "ok", "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (s *server) deviceMetrics(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.Store.GetDevice(r.Context(), name); err != nil {
		s.notFoundOr500(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Registry.Metrics(name))
}

// historySample is one point of GET /api/devices/{name}/metrics/history. T is
// the bucket start in unix seconds; GPUPct is absent when no GPU reported,
// GPUMemUsed when no GPU reported memory and GPUMemTotal when the total is
// unknown (e.g. Apple unified memory). TempC is absent when the device
// reported no host temperature.
type historySample struct {
	T           int64    `json:"t"`
	CPU         float64  `json:"cpu"`
	MemPct      float64  `json:"mem_pct"`
	GPUPct      *float64 `json:"gpu_pct,omitempty"`
	GPUMemUsed  *uint64  `json:"gpu_mem_used_bytes,omitempty"`
	GPUMemTotal *uint64  `json:"gpu_mem_total_bytes,omitempty"`
	DiskPct     float64  `json:"disk_pct"`
	TempC       *float64 `json:"temp_c,omitempty"`
}

// historyWindows are the supported ?window= values. 1h is served from the 30s
// tier; longer windows from 15-minute rollups.
var historyWindows = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": store.MetricsRetention,
}

func (s *server) deviceMetricsHistory(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "1h"
	}
	d, ok := historyWindows[window]
	if !ok {
		writeError(w, http.StatusBadRequest, "window must be 1h|24h|7d|30d")
		return
	}
	if _, err := s.Store.GetDevice(r.Context(), name); err != nil {
		s.notFoundOr500(w, err)
		return
	}
	query := s.Store.MetricsRollupHistory
	if window == "1h" {
		query = s.Store.MetricsHistory
	}
	rows, err := query(r.Context(), name, time.Now().Add(-d))
	if err != nil {
		s.Log.Error("metrics history", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	samples := make([]historySample, 0, len(rows))
	for _, m := range rows {
		hs := historySample{T: m.Bucket.Unix(), CPU: m.CPUPercent, GPUPct: m.GPUPercent,
			GPUMemUsed: m.GPUMemUsedBytes, GPUMemTotal: m.GPUMemTotalBytes, DiskPct: m.DiskUsedPercent, TempC: m.TempC}
		if m.MemTotalBytes > 0 {
			hs.MemPct = float64(m.MemUsedBytes) / float64(m.MemTotalBytes) * 100
		}
		samples = append(samples, hs)
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": name, "window": window, "samples": samples})
}

func (s *server) deviceCommands(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.GetDevice(r.Context(), r.PathValue("name"))
	if err != nil {
		s.notFoundOr500(w, err)
		return
	}
	cmds, err := s.Store.ListCommands(r.Context(), d.ID)
	if err != nil {
		s.Log.Error("list commands", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if cmds == nil {
		cmds = []proto.CommandDef{}
	}
	writeJSON(w, http.StatusOK, cmds)
}

func (s *server) notFoundOr500(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	s.Log.Error("internal error", "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func intQuery(r *http.Request, key string, def int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return def
	}
	return v
}
