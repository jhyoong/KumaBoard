package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"time"

	"github.com/jhyoong/KumaBoard/server/store"
)

const CookieName = "kb_session"
const SessionTTL = 12 * time.Hour

type Sessions struct {
	st *store.Store
}

func NewSessions(st *store.Store) *Sessions { return &Sessions{st: st} }

func (s *Sessions) Create(ctx context.Context, userID int64) (string, time.Time, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	id := base64.RawURLEncoding.EncodeToString(b)
	exp := time.Now().Add(SessionTTL)
	if err := s.st.CreateSession(ctx, id, userID, exp); err != nil {
		return "", time.Time{}, err
	}
	return id, exp, nil
}

func (s *Sessions) Valid(ctx context.Context, id string) bool {
	if id == "" {
		return false
	}
	_, exp, err := s.st.GetSession(ctx, id)
	return err == nil && time.Now().Before(exp)
}

func (s *Sessions) Delete(ctx context.Context, id string) error {
	return s.st.DeleteSession(ctx, id)
}

func SetCookie(w http.ResponseWriter, id string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: id, Path: "/", Expires: expires,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

func ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}
