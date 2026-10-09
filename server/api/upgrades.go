package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/jhyoong/KumaBoard/server/hub"
	"github.com/jhyoong/KumaBoard/server/store"
)

func (s *server) mountUpgrades(authed *http.ServeMux) {
	authed.HandleFunc("GET /api/devices/{name}/upgrades", s.listDeviceUpgrades)
	authed.HandleFunc("GET /api/devices/{name}/upgrades/{id}", s.getDeviceUpgrade)
	authed.HandleFunc("POST /api/devices/{name}/upgrades/retry", s.retryUpgrade)
	authed.HandleFunc("POST /api/devices/{name}/upgrades/{id}/abandon", s.abandonUpgrade)
}

// Bounds of ?limit= on the upgrade list.
const (
	defaultUpgradeLimit = 20
	maxUpgradeLimit     = 100
)

func (s *server) listDeviceUpgrades(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.GetDevice(r.Context(), r.PathValue("name"))
	if err != nil {
		s.notFoundOr500(w, err)
		return
	}
	limit := intQuery(r, "limit", defaultUpgradeLimit)
	if limit < 1 {
		limit = defaultUpgradeLimit
	}
	if limit > maxUpgradeLimit {
		limit = maxUpgradeLimit
	}
	upgrades, err := s.Store.GetDeviceUpgrades(r.Context(), d.ID, limit)
	if err != nil {
		s.Log.Error("list upgrades", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if upgrades == nil {
		upgrades = []store.Upgrade{}
	}
	writeJSON(w, http.StatusOK, upgrades)
}

// upgradeDetail is one upgrade with its timeline. Events is empty for an
// upgrade that predates the events table.
type upgradeDetail struct {
	*store.Upgrade
	Events []store.UpgradeEvent `json:"events"`
}

func (s *server) getDeviceUpgrade(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.GetDevice(r.Context(), r.PathValue("name"))
	if err != nil {
		s.notFoundOr500(w, err)
		return
	}
	u, err := s.Store.GetUpgrade(r.Context(), r.PathValue("id"))
	if err == nil && u.DeviceID != d.ID {
		// An id that exists but belongs to another device is not found.
		err = store.ErrNotFound
	}
	if err != nil {
		s.notFoundOr500(w, err)
		return
	}
	events, err := s.Store.ListUpgradeEvents(r.Context(), u.ID)
	if err != nil {
		s.Log.Error("list upgrade events", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, upgradeDetail{Upgrade: u, Events: events})
}

// retryRefused is the 409 body of retryUpgrade. Code is a dispatch code, or
// "not_needed". BlockingDevice is the device holding the fleet's upgrade
// slot when Code is in_flight.
type retryRefused struct {
	Error          string `json:"error"`
	Code           string `json:"code"`
	BlockingDevice string `json:"blocking_device,omitempty"`
}

func (s *server) retryUpgrade(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	d, err := s.Store.GetDevice(r.Context(), name)
	if err != nil {
		s.notFoundOr500(w, err)
		return
	}
	if d.DesiredAgentVersion == "" {
		writeError(w, http.StatusBadRequest, "no desired version set")
		return
	}
	// Explicit retry bypasses the failed-attempt gate that blocks automatic
	// re-offers (see hub.evaluateUpgrade); prior failed rows stay as history.
	id, err := s.Hub.RetryUpgrade(r.Context(), d)
	if err != nil {
		s.Store.Audit(r.Context(), s.actor(r), "upgrade_retry", name, "rejected", err.Error())
		body := retryRefused{Error: "retry not dispatched: " + err.Error(), Code: hub.DispatchCode(err)}
		var inFlight *hub.InFlightError
		switch {
		case errors.As(err, &inFlight):
			body.BlockingDevice = inFlight.Device
		case body.Code == "":
			body.Code = "not_needed"
		case body.Code == store.DispatchSendFailed:
			// Not one of the hub's decline reasons: keep its text in the log.
			s.Log.Error("retry upgrade", "device", name, "err", err)
			body.Error = "retry not dispatched: send failed"
		}
		writeJSON(w, http.StatusConflict, body)
		return
	}
	s.Store.Audit(r.Context(), s.actor(r), "upgrade_retry", name, "ok", d.DesiredAgentVersion)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "retry requested", "upgrade_id": id})
}

func (s *server) abandonUpgrade(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	id := r.PathValue("id")
	d, err := s.Store.GetDevice(r.Context(), name)
	if err != nil {
		s.notFoundOr500(w, err)
		return
	}
	// Abandon closes the row; it does not stop the agent. If the agent
	// later reports how the upgrade ended, that report replaces this.
	if err := s.Store.AbandonUpgrade(r.Context(), d.ID, id); err != nil {
		if errors.Is(err, store.ErrUpgradeNotInFlight) {
			s.Store.Audit(r.Context(), s.actor(r), "upgrade_abandon", name, "rejected", id)
			writeJSON(w, http.StatusConflict, map[string]string{"error": "upgrade is not in flight", "code": "not_in_flight"})
			return
		}
		s.notFoundOr500(w, err)
		return
	}
	s.Store.Audit(r.Context(), s.actor(r), "upgrade_abandon", name, "ok", id)
	s.Registry.Refresh(name)
	s.Hub.RefreshBlocked(r.Context())
	writeJSON(w, http.StatusOK, map[string]string{"status": "abandoned"})
}

// tryUpgrade offers d its desired version. The outcome is not returned: the
// hub stores it on the device, where the summary picks it up.
func (s *server) tryUpgrade(ctx context.Context, d *store.Device) {
	s.Hub.SendUpgradeToDevice(ctx, d)
}
