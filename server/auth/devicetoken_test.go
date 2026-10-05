package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jhyoong/KumaBoard/server/store"
)

func TestRequireDeviceToken(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, token, _ := st.CreateDevice(context.Background(), "test-dev", "", false, store.Schedule{})

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := RequireDeviceToken(st)(inner)

	// No headers: 401.
	r := httptest.NewRequest("GET", "/api/agent/releases/0.4.0/linux_amd64", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no headers: got %d", w.Code)
	}

	// Valid token: 200.
	r = httptest.NewRequest("GET", "/api/agent/releases/0.4.0/linux_amd64", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Device-Name", "test-dev")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("valid token: got %d", w.Code)
	}

	// Wrong token: 401.
	r = httptest.NewRequest("GET", "/api/agent/releases/0.4.0/linux_amd64", nil)
	r.Header.Set("Authorization", "Bearer wrong-token")
	r.Header.Set("X-Device-Name", "test-dev")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d", w.Code)
	}

	// Cookie instead of bearer: 401.
	r = httptest.NewRequest("GET", "/api/agent/releases/0.4.0/linux_amd64", nil)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: "some-session-id"})
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("cookie only: got %d", w.Code)
	}
}
