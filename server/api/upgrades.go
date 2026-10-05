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
	authed.HandleFunc("POST /api/devices/{name}/upgrades/retry", s.retryUpgrade)
	authed.HandleFunc("POST /api/devices/{name}/upgrades/{id}/abandon", s.abandonUpgrade)
}

func (s *server) listDeviceUpgrades(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.GetDevice(r.Context(), r.PathValue("name"))
	if err != nil {
		s.notFoundOr500(w, err)
		return
	}
	upgrades, err := s.Store.GetDeviceUpgrades(r.Context(), d.ID)
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
	if err := s.Hub.RetryUpgrade(r.Context(), d); err != nil {
		s.Store.Audit(r.Context(), s.actor(r), "upgrade_retry", name, "rejected", err.Error())
		if errors.Is(err, hub.ErrUpgradeNotNeeded) || errors.Is(err, hub.ErrUpgradeInFlight) ||
			errors.Is(err, hub.ErrUpgradeNoRelease) || errors.Is(err, hub.ErrUpgradeNotConnected) {
			writeError(w, http.StatusConflict, "retry not dispatched: "+err.Error())
			return
		}
		s.Log.Error("retry upgrade", "device", name, "err", err)
		writeError(w, http.StatusConflict, "retry not dispatched: send failed")
		return
	}
	s.Store.Audit(r.Context(), s.actor(r), "upgrade_retry", name, "ok", d.DesiredAgentVersion)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "retry requested"})
}

func (s *server) abandonUpgrade(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	id := r.PathValue("id")
	if _, err := s.Store.GetDevice(r.Context(), name); err != nil {
		s.notFoundOr500(w, err)
		return
	}
	if err := s.Store.AbandonUpgrade(r.Context(), id); err != nil {
		s.Log.Error("abandon upgrade", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.Store.Audit(r.Context(), s.actor(r), "upgrade_abandon", name, "ok", id)
	writeJSON(w, http.StatusOK, map[string]string{"status": "abandoned"})
}

func (s *server) tryUpgrade(ctx context.Context, d *store.Device) {
	s.Hub.SendUpgradeToDevice(ctx, d)
}
