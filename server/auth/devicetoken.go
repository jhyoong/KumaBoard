package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/jhyoong/KumaBoard/server/store"
)

func RequireDeviceToken(st *store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			deviceName := r.Header.Get("X-Device-Name")
			if deviceName == "" || !strings.HasPrefix(authHeader, "Bearer ") {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			token := strings.TrimPrefix(authHeader, "Bearer ")
			d, err := st.GetDevice(r.Context(), deviceName)
			if err != nil || d.TokenHash == nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if subtle.ConstantTimeCompare(store.HashToken(token), d.TokenHash) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
