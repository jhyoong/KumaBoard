package api

import (
	"errors"
	"net/http"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/jhyoong/KumaBoard/server/auth"
	"github.com/jhyoong/KumaBoard/server/terminal"
)

func (s *server) requestTerminal(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}
	if err := readJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}
	if body.Cols <= 0 {
		body.Cols = 80
	}
	if body.Rows <= 0 {
		body.Rows = 24
	}

	d, err := s.Store.GetDevice(r.Context(), name)
	if err != nil {
		s.notFoundOr500(w, err)
		return
	}
	if !d.TerminalEnabled {
		writeError(w, http.StatusForbidden, "terminal_not_enabled")
		return
	}
	hasCap := false
	for _, c := range d.Capabilities {
		if c == proto.CapTerminal {
			hasCap = true
			break
		}
	}
	if !hasCap {
		writeError(w, http.StatusForbidden, "terminal_not_capable")
		return
	}
	if !s.Hub.Connected(name) {
		writeError(w, http.StatusConflict, "not_connected")
		return
	}

	user, err := s.sessionUser(r)
	if err != nil {
		s.Log.Error("resolve session user", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// The broker is the single source of truth for pending and live sessions;
	// it enforces the per-device limit atomically.
	tk, err := s.Terminal.CreateTicket(name, d.ID, user, body.Cols, body.Rows)
	if err != nil {
		if errors.Is(err, terminal.ErrSessionLimit) {
			s.Store.Audit(r.Context(), user, "terminal_open", name, "failed", "session limit")
			writeError(w, http.StatusConflict, "session_limit")
			return
		}
		s.Log.Error("create terminal ticket", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Close the race with a concurrent disable or revoke: updateDevice and
	// revokeDevice write the device row before they call CloseDevice, so a
	// ticket issued after that sweep sees the change here.
	cur, err := s.Store.GetDevice(r.Context(), name)
	if err != nil || !cur.TerminalEnabled {
		s.Terminal.CancelTicket(tk.SessionID)
		writeError(w, http.StatusForbidden, "terminal_not_enabled")
		return
	}
	if len(cur.TokenHash) == 0 {
		s.Terminal.CancelTicket(tk.SessionID)
		writeError(w, http.StatusConflict, "not_connected")
		return
	}

	env, _ := proto.New(proto.TypeTerminalOpen, proto.TerminalOpen{
		SessionID:   tk.SessionID,
		AgentTicket: tk.AgentTicket,
		Cols:        body.Cols,
		Rows:        body.Rows,
	})
	sess := s.Hub.Session(name)
	if sess == nil {
		s.Terminal.CancelTicket(tk.SessionID)
		writeError(w, http.StatusConflict, "not_connected")
		return
	}
	if err := sess.Send(env); err != nil {
		s.Terminal.CancelTicket(tk.SessionID)
		writeError(w, http.StatusBadGateway, "agent_unreachable")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"ticket":     tk.BrowserTicket,
		"session_id": tk.SessionID,
	})
}

// sessionUser returns the username behind the request's dashboard session.
func (s *server) sessionUser(r *http.Request) (string, error) {
	c, err := r.Cookie(auth.CookieName)
	if err != nil {
		return "", err
	}
	return s.Store.SessionUsername(r.Context(), c.Value)
}

// actor returns the dashboard user to record in the audit log for r. It never
// fails the request: an unresolvable session is logged and recorded as
// "unknown" so the audit row is still written.
func (s *server) actor(r *http.Request) string {
	user, err := s.sessionUser(r)
	if err != nil || user == "" {
		s.Log.Error("resolve audit actor", "err", err)
		return "unknown"
	}
	return user
}
