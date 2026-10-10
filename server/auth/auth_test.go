package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jhyoong/KumaBoard/server/store"
)

func TestPasswordHashVerify(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword("correct horse", h) || VerifyPassword("wrong", h) || VerifyPassword("x", "garbage") {
		t.Fatal("verify wrong")
	}
}

func TestLimiterLocksAfterFiveFailures(t *testing.T) {
	now := time.Now()
	l := NewLimiter(5, time.Minute)
	l.now = func() time.Time { return now }
	for i := 0; i < 4; i++ {
		l.Fail("1.2.3.4")
		if !l.Allowed("1.2.3.4") {
			t.Fatalf("locked after %d failures", i+1)
		}
	}
	l.Fail("1.2.3.4")
	if l.Allowed("1.2.3.4") {
		t.Fatal("not locked after 5 failures")
	}
	if !l.Allowed("5.6.7.8") {
		t.Fatal("other ip affected")
	}
	now = now.Add(61 * time.Second)
	if !l.Allowed("1.2.3.4") {
		t.Fatal("still locked after lockout expired")
	}
	l.Fail("1.2.3.4")
	l.Reset("1.2.3.4")
	for i := 0; i < 4; i++ {
		l.Fail("1.2.3.4")
	}
	if !l.Allowed("1.2.3.4") {
		t.Fatal("reset did not clear failures")
	}
}

func TestSessionsAndMiddleware(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	ctx := context.Background()
	st.UpsertUser(ctx, "admin", "x")
	uid, _, _ := st.GetUser(ctx, "admin")
	s := NewSessions(st)
	id, _, err := s.Create(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := RequireSession(s)(ok)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/devices", nil))
	if rec.Code != 401 || rec.Body.Len() != 0 {
		t.Fatalf("no cookie: code=%d body=%q", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest("GET", "/api/devices", nil)
	req.AddCookie(&http.Cookie{Name: CookieName, Value: id})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("valid cookie: code=%d", rec.Code)
	}
	s.Delete(ctx, id)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatal("deleted session still valid")
	}
}

func TestHostAndOriginChecks(t *testing.T) {
	allowed := map[string]bool{"192.168.1.5:8443": true, "kuma.local": true}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := HostCheck(allowed)(ok)
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "evil.example:8443"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("bad host accepted: %d", rec.Code)
	}
	req.Host = "192.168.1.5:8443"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("good host rejected: %d", rec.Code)
	}

	o := OriginCheck(allowed)(ok)
	req = httptest.NewRequest("GET", "/api/events", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	o.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bad origin accepted: %d", rec.Code)
	}
	req.Header.Set("Origin", "https://192.168.1.5:8443")
	rec = httptest.NewRecorder()
	o.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("good origin rejected: %d", rec.Code)
	}
	req.Header.Del("Origin")
	rec = httptest.NewRecorder()
	o.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("missing origin rejected: %d", rec.Code)
	}
}

// Reserve counts the attempt up front, so attempts made in parallel cannot
// all pass before any failure is recorded.
func TestLimiterReserveBoundsParallelAttempts(t *testing.T) {
	l := NewLimiter(5, time.Minute)
	var wg sync.WaitGroup
	var passed atomic.Int32
	for range 100 {
		wg.Go(func() {
			if l.Reserve("1.2.3.4") {
				passed.Add(1)
			}
		})
	}
	wg.Wait()
	if n := passed.Load(); n != 5 {
		t.Fatalf("%d attempts passed, want 5", n)
	}
	if l.Allowed("1.2.3.4") {
		t.Fatal("not locked after 5 reserved attempts")
	}
	// A success on the last allowed attempt clears the lockout it set.
	l = NewLimiter(5, time.Minute)
	for range 5 {
		l.Reserve("1.2.3.4")
	}
	l.Reset("1.2.3.4")
	if !l.Allowed("1.2.3.4") {
		t.Fatal("reset did not clear the reserved attempts")
	}
}

func TestLimiterFailuresDecay(t *testing.T) {
	now := time.Now()
	l := NewLimiter(5, time.Minute)
	l.now = func() time.Time { return now }
	for range 4 {
		l.Fail("1.2.3.4")
	}
	now = now.Add(2 * time.Minute)
	l.Fail("1.2.3.4")
	if !l.Allowed("1.2.3.4") {
		t.Fatal("failures older than the lockout window still counted")
	}
	now = now.Add(2 * time.Minute)
	l.Sweep()
	if len(l.entries) != 0 {
		t.Fatalf("sweep left %d entries", len(l.entries))
	}
}

func TestLimiterKeysIPv6BySlash64(t *testing.T) {
	l := NewLimiter(5, time.Minute)
	for i := range 5 {
		l.Fail(fmt.Sprintf("2001:db8:1:2::%x", i+1))
	}
	if l.Allowed("2001:db8:1:2:ffff::1") {
		t.Fatal("another address in the same /64 is not locked")
	}
	if !l.Allowed("2001:db8:1:3::1") {
		t.Fatal("a different /64 is locked")
	}
	// An IPv4-mapped address is the IPv4 client, not a /64.
	for range 5 {
		l.Fail("::ffff:10.0.0.1")
	}
	if l.Allowed("10.0.0.1") || !l.Allowed("10.0.0.2") {
		t.Fatal("IPv4-mapped address not keyed as its IPv4 address")
	}
}
