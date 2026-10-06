package api

import (
	"errors"
	"net/http"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/hub"
	"github.com/jhyoong/KumaBoard/server/store"
)

func (s *server) mountRuns(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/devices/{name}/commands/{cmd}", s.runCommand)
	mux.HandleFunc("GET /api/devices/{name}/runs", s.listRuns)
	mux.HandleFunc("GET /api/runs/{id}", s.getRun)
	mux.HandleFunc("POST /api/runs/{id}/cancel", s.cancelRun)
}

// overlayLive fills a run's output from the hub's in-flight buffers while
// it is still running: nothing is stored until it finishes. A page opened
// mid-run then starts from the current tail and follows run_output events
// with a seq above OutputSeq.
func (s *server) overlayLive(run *store.Run) {
	if proto.IsTerminalRunStatus(run.Status) {
		return
	}
	if stdout, stderr, truncated, seq, ok := s.Hub.LiveOutput(run.ID); ok {
		run.StdoutTail, run.StderrTail, run.Truncated, run.OutputSeq = stdout, stderr, truncated, seq
	}
}

// cancelRun asks the agent to stop a run. It carries nothing but the run ID
// from the path; the body is not read.
func (s *server) cancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetRun(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		s.Log.Error("cancel run", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	err := s.Hub.CancelRun(r.Context(), id, s.actor(r))
	switch {
	case errors.Is(err, hub.ErrRunNotInFlight):
		writeError(w, http.StatusConflict, "run is not in flight")
	case errors.Is(err, hub.ErrCancelUnsupported):
		writeError(w, http.StatusConflict, "agent does not support cancel; upgrade it to protocol 2")
	case err != nil:
		s.Log.Error("cancel run", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"run_id": id, "status": "cancel_sent"})
	}
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
	for _, run := range runs {
		s.overlayLive(run)
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
	s.overlayLive(run)
	writeJSON(w, http.StatusOK, run)
}
