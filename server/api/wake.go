package api

import "net/http"

func (s *server) mountWake(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/devices/{name}/wake", s.wakeDevice)
}

func (s *server) wakeDevice(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	d, err := s.Store.GetDevice(r.Context(), name)
	if err != nil {
		s.notFoundOr500(w, err)
		return
	}
	if d.MAC == "" {
		writeError(w, http.StatusBadRequest, "device has no MAC address configured")
		return
	}
	if s.Wake == nil {
		writeError(w, http.StatusServiceUnavailable, "wake-on-lan is not configured on the server")
		return
	}
	if err := s.Wake(d.MAC); err != nil {
		s.Store.Audit(r.Context(), s.actor(r), "wake", name, "failed", err.Error())
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.Store.Audit(r.Context(), s.actor(r), "wake", name, "sent", d.MAC)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent"})
}
