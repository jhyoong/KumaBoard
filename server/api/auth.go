package api

import (
	"net/http"

	"github.com/jhyoong/KumaBoard/server/auth"
	"github.com/jhyoong/KumaBoard/server/store"
	"github.com/jhyoong/KumaBoard/server/terminal"
)

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.Limiter.Allowed(ip) {
		s.Store.Audit(r.Context(), ip, "login", "", "locked_out", "")
		writeError(w, http.StatusTooManyRequests, "too many failed logins; try again later")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}
	id, hash, err := s.Store.GetUser(r.Context(), body.Username)
	if err != nil || !auth.VerifyPassword(body.Password, hash) {
		if err != nil && err != store.ErrNotFound {
			s.Log.Error("get user", "err", err)
		}
		s.Limiter.Fail(ip)
		s.Store.Audit(r.Context(), ip, "login", body.Username, "failed", "")
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	sid, exp, err := s.Sessions.Create(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session error")
		return
	}
	s.Limiter.Reset(ip)
	s.Store.Audit(r.Context(), body.Username, "login", body.Username, "ok", ip)
	auth.SetCookie(w, sid, exp)
	writeJSON(w, http.StatusOK, map[string]string{"username": body.Username})
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	// Resolve the user before the session row is deleted.
	user := s.actor(r)
	if c, err := r.Cookie(auth.CookieName); err == nil {
		s.Sessions.Delete(r.Context(), c.Value)
	}
	// Logging out ends that user's web shells and voids their pending tickets.
	if user != "unknown" && s.Terminal != nil {
		s.Terminal.CloseUser(user, terminal.ReasonLoggedOut)
	}
	auth.ClearCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"username": "admin"})
}

func (s *server) listAudit(w http.ResponseWriter, r *http.Request) {
	limit := intQuery(r, "limit", 100)
	before := int64(intQuery(r, "before", 0))
	entries, err := s.Store.ListAudit(r.Context(), limit, before)
	if err != nil {
		s.Log.Error("list audit", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if entries == nil {
		entries = []store.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}
