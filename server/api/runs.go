package api

import (
	"errors"
	"net/http"

	"github.com/jhyoong/KumaBoard/server/hub"
	"github.com/jhyoong/KumaBoard/server/store"
)

func (s *server) mountRuns(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/devices/{name}/commands/{cmd}", s.runCommand)
	mux.HandleFunc("GET /api/devices/{name}/runs", s.listRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.getRun)
}

func (s *server) runCommand(w http.ResponseWriter, r *http.Request) {
	name, cmd := r.PathValue("name"), r.PathValue("cmd")
	id, err := s.Hub.RequestCommand(r.Context(), name, cmd, "admin")
	switch {
	case errors.Is(err, hub.ErrNotConnected):
		writeError(w, http.StatusConflict, "device is not connected")
	case errors.Is(err, hub.ErrUnknownCommand):
		writeError(w, http.StatusNotFound, "command not declared by device")
	case err != nil:
		s.Log.Error("run command", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"run_id": id})
	}
}

func (s *server) listRuns(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.GetDevice(r.Context(), r.PathValue("name"))
	if err != nil {
		s.notFoundOr500(w, err)
		return
	}
	runs, err := s.Store.ListRuns(r.Context(), d.ID, intQuery(r, "limit", 50))
	if err != nil {
		s.Log.Error("list runs", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *server) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.Store.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		s.Log.Error("get run", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, run)
}
