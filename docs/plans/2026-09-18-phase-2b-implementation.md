# Phase 2B: Agent Upgrades -- Server, Dashboard, and Integration

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build the server-side upgrade orchestration, device-token auth, dashboard UI, and integration tests.

**Architecture:** The server evaluates upgrade eligibility at handshake and immediately when a target version is set. One upgrade in flight at a time across the fleet. The dashboard exposes a per-device upgrade panel with target selection, retry, and abandon.

**Tech Stack:** Go standard library, SQLite, React + TypeScript + Tailwind.

**Prerequisite:** Phase 2A tasks 1 (signing) and 4 (pending marker) must be completed first. Tasks 9 and 10 from this plan have no dependencies on 2A and can start in parallel.

---

### Task 9: Store upgrade and release operations

**Files:**
- Create: `server/store/upgrades.go`
- Create: `server/store/upgrades_test.go`

The `agent_upgrades` and `releases` tables already exist in `0001_init.sql`.

**Step 1: Write the failing tests**

```go
// server/store/upgrades_test.go
package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jhyoong/KumaBoard/proto"
)

func openTestDB(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestIngestAndListReleases(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()
	err := st.IngestRelease(ctx, "0.4.0", "linux", "amd64", "abc123", "sig1", 1000)
	if err != nil {
		t.Fatal(err)
	}
	err = st.IngestRelease(ctx, "0.4.0", "darwin", "arm64", "def456", "sig2", 2000)
	if err != nil {
		t.Fatal(err)
	}
	all, err := st.ListReleases(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d releases", len(all))
	}
	filtered, err := st.ListReleases(ctx, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].Version != "0.4.0" {
		t.Fatalf("filtered = %+v", filtered)
	}
}

func TestGetRelease(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()
	st.IngestRelease(ctx, "0.4.0", "linux", "amd64", "abc", "sig", 100)
	r, err := st.GetRelease(ctx, "0.4.0", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if r.SHA256 != "abc" {
		t.Fatalf("sha256 = %q", r.SHA256)
	}
}

func TestCreateAndUpdateUpgrade(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()
	st.CreateDevice(ctx, "test-dev", "", false, Schedule{})
	d, _ := st.GetDevice(ctx, "test-dev")
	id, err := st.CreateUpgrade(ctx, d.ID, "0.3.0", "0.4.0", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateUpgradeState(ctx, id, proto.UpgradeDownloading, ""); err != nil {
		t.Fatal(err)
	}
	u, err := st.GetLatestUpgrade(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.State != proto.UpgradeDownloading {
		t.Fatalf("state = %q", u.State)
	}
}

func TestGetActiveUpgrade(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()
	st.CreateDevice(ctx, "dev-a", "", false, Schedule{})
	d, _ := st.GetDevice(ctx, "dev-a")
	id, _ := st.CreateUpgrade(ctx, d.ID, "0.3.0", "0.4.0", "admin")
	active, err := st.GetActiveUpgrade(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if active == nil || active.ID != id {
		t.Fatal("expected active upgrade")
	}
	st.UpdateUpgradeState(ctx, id, proto.UpgradeVerified, "")
	active, _ = st.GetActiveUpgrade(ctx)
	if active != nil {
		t.Fatal("expected no active upgrade after verified")
	}
}

func TestHasFailedUpgrade(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()
	st.CreateDevice(ctx, "dev-b", "", false, Schedule{})
	d, _ := st.GetDevice(ctx, "dev-b")
	id, _ := st.CreateUpgrade(ctx, d.ID, "0.3.0", "0.4.0", "admin")
	st.UpdateUpgradeState(ctx, id, proto.UpgradeFailed, "verify_failed")
	if !st.HasFailedUpgrade(ctx, d.ID, "0.4.0") {
		t.Fatal("expected failed upgrade")
	}
	if st.HasFailedUpgrade(ctx, d.ID, "0.5.0") {
		t.Fatal("unexpected failed upgrade for different version")
	}
}

func TestAbandonUpgrade(t *testing.T) {
	st := openTestDB(t)
	ctx := context.Background()
	st.CreateDevice(ctx, "dev-c", "", false, Schedule{})
	d, _ := st.GetDevice(ctx, "dev-c")
	id, _ := st.CreateUpgrade(ctx, d.ID, "0.3.0", "0.4.0", "admin")
	if err := st.AbandonUpgrade(ctx, id); err != nil {
		t.Fatal(err)
	}
	u, _ := st.GetLatestUpgrade(ctx, d.ID)
	if u.State != proto.UpgradeFailed || u.FailureReason != "abandoned" {
		t.Fatalf("state=%q reason=%q", u.State, u.FailureReason)
	}
}
```

**Step 2: Run tests to verify they fail**

Run: `cd /path/to/KumaBoard && go test ./server/store/ -v -run "TestIngest|TestGetRelease|TestCreate.*Upgrade|TestGetActive|TestHasFailed|TestAbandon"`
Expected: compilation error

**Step 3: Write the implementation**

```go
// server/store/upgrades.go
package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/jhyoong/KumaBoard/proto"
	"github.com/oklog/ulid/v2"
)

// Release is one row of the releases table.
type Release struct {
	Version   string    `json:"version"`
	OS        string    `json:"os"`
	Arch      string    `json:"arch"`
	SHA256    string    `json:"sha256"`
	Signature string    `json:"signature"`
	SizeBytes int64     `json:"size_bytes"`
	AddedAt   time.Time `json:"added_at"`
}

// IngestRelease inserts a release row. Replaces on conflict.
func (s *Store) IngestRelease(ctx context.Context, version, os, arch, sha256, signature string, sizeBytes int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO releases (version, os, arch, sha256, signature, size_bytes, added_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(version, os, arch) DO UPDATE SET sha256=?, signature=?, size_bytes=?, added_at=?`,
		version, os, arch, sha256, signature, sizeBytes, nowString(),
		sha256, signature, sizeBytes, nowString())
	return err
}

// GetRelease returns one release by version and platform.
func (s *Store) GetRelease(ctx context.Context, version, os, arch string) (*Release, error) {
	var r Release
	var addedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT version, os, arch, sha256, signature, size_bytes, added_at FROM releases WHERE version=? AND os=? AND arch=?`,
		version, os, arch).Scan(&r.Version, &r.OS, &r.Arch, &r.SHA256, &r.Signature, &r.SizeBytes, &addedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.AddedAt = mustTime(addedAt)
	return &r, nil
}

// ListReleases returns releases, optionally filtered by os and arch.
func (s *Store) ListReleases(ctx context.Context, os, arch string) ([]Release, error) {
	var query string
	var args []any
	if os != "" && arch != "" {
		query = `SELECT version, os, arch, sha256, signature, size_bytes, added_at FROM releases WHERE os=? AND arch=? ORDER BY added_at DESC`
		args = []any{os, arch}
	} else {
		query = `SELECT version, os, arch, sha256, signature, size_bytes, added_at FROM releases ORDER BY added_at DESC`
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		var r Release
		var addedAt string
		if err := rows.Scan(&r.Version, &r.OS, &r.Arch, &r.SHA256, &r.Signature, &r.SizeBytes, &addedAt); err != nil {
			return nil, err
		}
		r.AddedAt = mustTime(addedAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Upgrade is one row of the agent_upgrades table.
type Upgrade struct {
	ID            string     `json:"id"`
	DeviceID      int64      `json:"device_id"`
	FromVersion   string     `json:"from_version"`
	ToVersion     string     `json:"to_version"`
	RequestedBy   string     `json:"requested_by"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
	State         string     `json:"state"`
	FailureReason string     `json:"failure_reason"`
}

// IsInFlight reports whether the upgrade state is not terminal.
func (u *Upgrade) IsInFlight() bool {
	switch u.State {
	case proto.UpgradeVerified, proto.UpgradeRolledBack, proto.UpgradeFailed:
		return false
	}
	return true
}

// CreateUpgrade inserts a new upgrade row with state "requested".
func (s *Store) CreateUpgrade(ctx context.Context, deviceID int64, fromVersion, toVersion, requestedBy string) (string, error) {
	id := ulid.Make().String()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO agent_upgrades (id, device_id, from_version, to_version, requested_by, started_at, state)
		 VALUES (?, ?, ?, ?, ?, ?, 'requested')`,
		id, deviceID, fromVersion, toVersion, requestedBy, nowString())
	return id, err
}

// UpdateUpgradeState sets the state and optional reason. Terminal states set finished_at.
func (s *Store) UpdateUpgradeState(ctx context.Context, id, state, reason string) error {
	var finishedAt any
	switch state {
	case proto.UpgradeVerified, proto.UpgradeRolledBack, proto.UpgradeFailed:
		now := nowString()
		finishedAt = now
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE agent_upgrades SET state=?, failure_reason=?, finished_at=COALESCE(?, finished_at) WHERE id=?`,
		state, reason, finishedAt, id)
	return err
}

// GetActiveUpgrade returns any upgrade row in the fleet that is not terminal.
func (s *Store) GetActiveUpgrade(ctx context.Context) (*Upgrade, error) {
	var u Upgrade
	var startedAt string
	var finishedAt sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, device_id, from_version, to_version, requested_by, started_at, finished_at, state, failure_reason
		 FROM agent_upgrades
		 WHERE state NOT IN (?, ?, ?)
		 LIMIT 1`,
		proto.UpgradeVerified, proto.UpgradeRolledBack, proto.UpgradeFailed,
	).Scan(&u.ID, &u.DeviceID, &u.FromVersion, &u.ToVersion, &u.RequestedBy,
		&startedAt, &finishedAt, &u.State, &u.FailureReason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.StartedAt = mustTime(startedAt)
	u.FinishedAt = parseTime(finishedAt)
	return &u, nil
}

// GetLatestUpgrade returns the most recent upgrade for a device.
func (s *Store) GetLatestUpgrade(ctx context.Context, deviceID int64) (*Upgrade, error) {
	var u Upgrade
	var startedAt string
	var finishedAt sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, device_id, from_version, to_version, requested_by, started_at, finished_at, state, failure_reason
		 FROM agent_upgrades WHERE device_id=? ORDER BY started_at DESC LIMIT 1`,
		deviceID).Scan(&u.ID, &u.DeviceID, &u.FromVersion, &u.ToVersion, &u.RequestedBy,
		&startedAt, &finishedAt, &u.State, &u.FailureReason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.StartedAt = mustTime(startedAt)
	u.FinishedAt = parseTime(finishedAt)
	return &u, nil
}

// GetDeviceUpgrades returns all upgrades for a device, newest first.
func (s *Store) GetDeviceUpgrades(ctx context.Context, deviceID int64) ([]Upgrade, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, device_id, from_version, to_version, requested_by, started_at, finished_at, state, failure_reason
		 FROM agent_upgrades WHERE device_id=? ORDER BY started_at DESC`,
		deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Upgrade
	for rows.Next() {
		var u Upgrade
		var startedAt string
		var finishedAt sql.NullString
		if err := rows.Scan(&u.ID, &u.DeviceID, &u.FromVersion, &u.ToVersion, &u.RequestedBy,
			&startedAt, &finishedAt, &u.State, &u.FailureReason); err != nil {
			return nil, err
		}
		u.StartedAt = mustTime(startedAt)
		u.FinishedAt = parseTime(finishedAt)
		out = append(out, u)
	}
	return out, rows.Err()
}

// HasFailedUpgrade reports whether there is a rolled_back or failed row for
// this device and target version.
func (s *Store) HasFailedUpgrade(ctx context.Context, deviceID int64, toVersion string) bool {
	var n int
	s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agent_upgrades WHERE device_id=? AND to_version=? AND state IN (?, ?)`,
		deviceID, toVersion, proto.UpgradeRolledBack, proto.UpgradeFailed).Scan(&n)
	return n > 0
}

// AbandonUpgrade marks an in-flight upgrade as failed(abandoned).
func (s *Store) AbandonUpgrade(ctx context.Context, id string) error {
	return s.UpdateUpgradeState(ctx, id, proto.UpgradeFailed, "abandoned")
}

// SetDesiredAgentVersion sets the target version for a device.
func (s *Store) SetDesiredAgentVersion(ctx context.Context, name, version string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET desired_agent_version=? WHERE name=?`, version, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
```

**Step 4: Run tests**

Run: `cd /path/to/KumaBoard && go test ./server/store/ -v -run "TestIngest|TestGetRelease|TestCreate.*Upgrade|TestGetActive|TestHasFailed|TestAbandon"`
Expected: all PASS

**Step 5: Commit**

```bash
git add server/store/upgrades.go server/store/upgrades_test.go
git commit -m "feat(store): upgrade and release CRUD operations"
```

---

### Task 10: Device token auth middleware

**Files:**
- Create: `server/auth/devicetoken.go`
- Create: `server/auth/devicetoken_test.go`

**Step 1: Write the failing tests**

```go
// server/auth/devicetoken_test.go
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
```

**Step 2: Run tests to verify they fail**

Run: `cd /path/to/KumaBoard && go test ./server/auth/ -v -run TestRequireDeviceToken`
Expected: compilation error

**Step 3: Write the implementation**

```go
// server/auth/devicetoken.go
package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/jhyoong/KumaBoard/server/store"
)

// RequireDeviceToken returns middleware that authenticates requests by
// device token (Authorization: Bearer + X-Device-Name).
// Dashboard cookies are explicitly NOT valid here.
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
```

**Step 4: Run tests**

Run: `cd /path/to/KumaBoard && go test ./server/auth/ -v -run TestRequireDeviceToken`
Expected: all PASS

**Step 5: Commit**

```bash
git add server/auth/devicetoken.go server/auth/devicetoken_test.go
git commit -m "feat(auth): device token middleware for artifact downloads"
```

---

### Task 11: Artifact serving and release list API

**Files:**
- Create: `server/api/releases.go`
- Modify: `server/api/api.go` (mount the new routes)
- Modify: `cmd/kumaboard/serve.go` (pass data dir to API deps)

**Step 1: Write the releases API handler**

```go
// server/api/releases.go
package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (s *server) mountReleases(authed *http.ServeMux) {
	authed.HandleFunc("GET /api/releases", s.listReleases)
	authed.HandleFunc("GET /api/devices/{name}/upgrades", s.listDeviceUpgrades)
}

func (s *server) listReleases(w http.ResponseWriter, r *http.Request) {
	os_ := r.URL.Query().Get("os")
	arch := r.URL.Query().Get("arch")
	releases, err := s.Store.ListReleases(r.Context(), os_, arch)
	if err != nil {
		s.Log.Error("list releases", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if releases == nil {
		releases = []store.Release{}
	}
	writeJSON(w, http.StatusOK, releases)
}

func (s *server) serveArtifact(w http.ResponseWriter, r *http.Request) {
	version := r.PathValue("version")
	osArch := r.PathValue("os_arch")
	parts := strings.SplitN(osArch, "_", 2)
	if len(parts) != 2 {
		writeError(w, http.StatusBadRequest, "path must be version/os_arch")
		return
	}
	os_, arch := parts[0], parts[1]
	rel, err := s.Store.GetRelease(r.Context(), version, os_, arch)
	if err != nil {
		s.notFoundOr500(w, err)
		return
	}
	path := filepath.Join(s.ReleasesDir, version, osArch, artifactName(os_))
	f, err := os.Open(path)
	if err != nil {
		s.Log.Error("open artifact", "path", path, "err", err)
		writeError(w, http.StatusNotFound, "artifact not found on disk")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(rel.SizeBytes, 10))
	http.ServeContent(w, r, "", rel.AddedAt, f)
}

func artifactName(os_ string) string {
	if os_ == "windows" {
		return "kuma-agent.exe"
	}
	return "kuma-agent"
}
```

**Step 2: Add `ReleasesDir` to `api.Deps`**

In `server/api/api.go`, add `ReleasesDir string` to the `Deps` struct.

Add the `store` import (`"github.com/jhyoong/KumaBoard/server/store"`). Then
mount the new routes in `New`:

```go
// In New(), add to authed mux:
	s.mountReleases(authed)

// Add device-token-authed artifact route separately:
	agentMux := http.NewServeMux()
	agentMux.HandleFunc("GET /api/agent/releases/{version}/{os_arch}", s.serveArtifact)
	root.Handle("/api/agent/", auth.RequireDeviceToken(d.Store)(agentMux))
```

**Step 3: Pass ReleasesDir from serve.go**

In `cmd/kumaboard/serve.go` `buildHandler`, set `ReleasesDir`:

```go
	apiHandler := api.New(api.Deps{
		// ... existing fields ...
		ReleasesDir: filepath.Join(cfg.DataDir, "releases"),
	})
```

**Step 4: Verify it compiles**

Run: `cd /path/to/KumaBoard && go build ./cmd/kumaboard/`
Expected: no errors

**Step 5: Commit**

```bash
git add server/api/releases.go server/api/api.go cmd/kumaboard/serve.go
git commit -m "feat(api): artifact serving and release list endpoints"
```

---

### Task 12: Upgrade API endpoints

**Files:**
- Create: `server/api/upgrades.go`
- Modify: `server/api/api.go` (mount routes)
- Modify: `server/api/devices.go` (extend updateDevice for desired_agent_version)

**Step 1: Write the upgrade API handlers**

```go
// server/api/upgrades.go
package api

import (
	"errors"
	"net/http"

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
	s.Store.Audit(r.Context(), "admin", "upgrade_retry", name, "ok", d.DesiredAgentVersion)
	s.tryUpgrade(r.Context(), d)
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
	s.Store.Audit(r.Context(), "admin", "upgrade_abandon", name, "ok", id)
	writeJSON(w, http.StatusOK, map[string]string{"status": "abandoned"})
}
```

**Step 2: Extend updateDevice to accept desired_agent_version**

In `server/api/devices.go`, add `DesiredAgentVersion *string` to `deviceBody`:

```go
type deviceBody struct {
	Name                string         `json:"name"`
	MAC                 string         `json:"mac"`
	NormallyOff         bool           `json:"normally_off"`
	Schedule            store.Schedule `json:"schedule"`
	DesiredAgentVersion *string        `json:"desired_agent_version,omitempty"`
}
```

In `updateDevice`, after the existing `UpdateDeviceSettings` call, add:

```go
	if body.DesiredAgentVersion != nil {
		if err := s.Store.SetDesiredAgentVersion(r.Context(), name, *body.DesiredAgentVersion); err != nil {
			s.notFoundOr500(w, err)
			return
		}
		s.Store.Audit(r.Context(), "admin", "set_desired_version", name, "ok", *body.DesiredAgentVersion)
		d, _ := s.Store.GetDevice(r.Context(), name)
		if d != nil {
			s.tryUpgrade(r.Context(), d)
		}
	}
```

**Step 3: Stub `tryUpgrade` on server (implemented in Task 13)**

Add a temporary stub to `upgrades.go`:

```go
// tryUpgrade evaluates upgrade eligibility and sends the request if conditions hold.
// Implemented in Task 13 when hub integration is wired.
func (s *server) tryUpgrade(ctx context.Context, d *store.Device) {
	// Stub: will be filled when hub upgrade trigger is implemented.
}
```

Add `"context"` import.

**Step 4: Mount routes in api.go**

In `New()`, replace the duplicate `listDeviceUpgrades` mount from Task 11
(which was in `mountReleases`) and add:

```go
	s.mountUpgrades(authed)
```

Remove `listDeviceUpgrades` from `mountReleases` since it's now in `mountUpgrades`.

**Step 5: Verify it compiles**

Run: `cd /path/to/KumaBoard && go build ./cmd/kumaboard/`
Expected: no errors

**Step 6: Commit**

```bash
git add server/api/upgrades.go server/api/devices.go server/api/api.go server/api/releases.go
git commit -m "feat(api): upgrade endpoints and desired_agent_version in device update"
```

---

### Task 13: Hub upgrade trigger and result handler

**Files:**
- Modify: `server/hub/hub.go`
- Modify: `server/api/upgrades.go` (fill `tryUpgrade`)

**Step 1: Add upgrade_result handler to hub**

In `hub.go` `handleMessage`, add a case for `TypeUpgradeResult`:

```go
	case proto.TypeUpgradeResult:
		h.handleUpgradeResult(ctx, s, env)
```

Write `handleUpgradeResult`:

```go
func (h *Hub) handleUpgradeResult(ctx context.Context, s *Session, env *proto.Envelope) {
	var res proto.UpgradeResult
	if err := env.Unmarshal(&res); err != nil {
		return
	}
	u, err := h.opts.Store.GetLatestUpgrade(ctx, s.DeviceID)
	if err != nil || u == nil {
		h.opts.Log.Warn("upgrade result for unknown upgrade", "device", s.DeviceName)
		return
	}
	h.opts.Store.UpdateUpgradeState(ctx, u.ID, res.State, res.Reason)
	h.opts.Store.Audit(ctx, "agent:"+s.DeviceName, "upgrade_state", s.DeviceName,
		res.State, res.FromVersion+" -> "+res.ToVersion)
	h.opts.Events.UpgradeChanged(s.DeviceName)
}
```

**Step 2: Add UpgradeChanged to Events interface**

In `hub.go`, add to the `Events` interface:

```go
	UpgradeChanged(name string)
```

In the registry, implement it as a no-op for now (just republish the device):

```go
// In server/registry/registry.go:
func (r *Registry) UpgradeChanged(name string) {
	r.publishDevice(context.Background(), name)
}
```

In the integration test recorder, add:

```go
func (r *recorder) UpgradeChanged(name string) { r.add("upgrade:" + name) }
```

**Step 3: Add upgrade trigger after handshake**

In `hub.go` `serve`, after `h.opts.Events.DeviceConnected(sess.DeviceName)`,
add the upgrade check:

```go
	go h.checkUpgradeAfterHandshake(ctx, sess, hello.AgentVersion)
```

Write the check:

```go
func (h *Hub) checkUpgradeAfterHandshake(ctx context.Context, s *Session, agentVersion string) {
	d, err := h.opts.Store.GetDeviceByID(ctx, s.DeviceID)
	if err != nil {
		return
	}
	// If device handshakes at from_version while upgrade is in-flight, mark rolled back.
	u, _ := h.opts.Store.GetLatestUpgrade(ctx, d.ID)
	if u != nil && u.IsInFlight() && agentVersion == u.FromVersion {
		h.opts.Store.UpdateUpgradeState(ctx, u.ID, proto.UpgradeRolledBack, "handshake_at_from_version")
		h.opts.Store.Audit(ctx, "server", "upgrade_rollback_detected", d.Name, proto.UpgradeRolledBack, u.ID)
		return
	}
	h.evaluateUpgrade(ctx, d, s)
}
```

**Step 4: Write evaluateUpgrade and SendUpgradeRequest**

```go
// evaluateUpgrade checks eligibility and sends upgrade_request if conditions hold.
func (h *Hub) evaluateUpgrade(ctx context.Context, d *store.Device, s *Session) {
	if d.DesiredAgentVersion == "" || d.DesiredAgentVersion == d.AgentVersion {
		return
	}
	active, _ := h.opts.Store.GetActiveUpgrade(ctx)
	if active != nil {
		return
	}
	if h.opts.Store.HasFailedUpgrade(ctx, d.ID, d.DesiredAgentVersion) {
		return
	}
	rel, err := h.opts.Store.GetRelease(ctx, d.DesiredAgentVersion, d.OS, d.Arch)
	if err != nil {
		h.opts.Log.Warn("no release for target version", "device", d.Name,
			"version", d.DesiredAgentVersion, "os", d.OS, "arch", d.Arch)
		return
	}
	id, err := h.opts.Store.CreateUpgrade(ctx, d.ID, d.AgentVersion, d.DesiredAgentVersion, "server")
	if err != nil {
		h.opts.Log.Error("create upgrade row", "err", err)
		return
	}
	// Build the download URL.
	url := fmt.Sprintf("/api/agent/releases/%s/%s_%s", rel.Version, rel.OS, rel.Arch)
	req := proto.UpgradeRequest{
		Version: rel.Version, OS: rel.OS, Arch: rel.Arch,
		SHA256: rel.SHA256, SizeBytes: rel.SizeBytes, Signature: rel.Signature,
		URL: url,
	}
	env, _ := proto.New(proto.TypeUpgradeRequest, req)
	if err := s.Send(env); err != nil {
		h.opts.Log.Error("send upgrade request", "device", d.Name, "err", err)
		return
	}
	h.opts.Store.Audit(ctx, "server", "upgrade_request", d.Name, "sent", id)
	h.opts.Log.Info("upgrade request sent", "device", d.Name, "to", d.DesiredAgentVersion)
}

// SendUpgradeToDevice is called by the API when target version changes or Retry.
func (h *Hub) SendUpgradeToDevice(ctx context.Context, d *store.Device) {
	s := h.session(d.Name)
	if s == nil {
		return
	}
	h.evaluateUpgrade(ctx, d, s)
}
```

Add `"fmt"` to hub.go imports.

**Step 5: Fill tryUpgrade in server/api/upgrades.go**

```go
func (s *server) tryUpgrade(ctx context.Context, d *store.Device) {
	s.Hub.SendUpgradeToDevice(ctx, d)
}
```

**Step 6: URL fix for upgrade requests**

The URL in `upgrade_request` is a relative path. The agent needs a full URL to download from. The hub needs to know the server's base URL. Two options:
- Add a `BaseURL` field to hub Options
- Have the agent construct the full URL from its existing server URL config

The agent already has `cfg.Server.URL` which is `wss://host:port/ws`. The
simplest approach: the hub sends a relative URL (`/api/agent/releases/...`)
and the agent constructs the full URL by replacing the scheme and path of
its server URL. Alternatively, make the URL absolute in the hub.

Decision: send a relative URL, let the agent prepend its server base. This
avoids configuring the base URL on the hub. In `agent/upgrade/upgrade.go`
`HandleUpgradeRequest`, prepend the base:

```go
// In HandleUpgradeRequest, before download:
	downloadURL := req.URL
	if !strings.HasPrefix(downloadURL, "http") {
		// Relative URL from server; prepend our base.
		downloadURL = u.BaseURL + downloadURL
	}
```

Add `BaseURL string` to the `Upgrader` struct (set from the agent's server URL,
converted from `wss://` to `https://`).

**Step 7: Verify it compiles and tests pass**

Run: `cd /path/to/KumaBoard && go build ./... && go test ./...`
Expected: all PASS

**Step 8: Commit**

```bash
git add server/hub/hub.go server/registry/registry.go server/api/upgrades.go \
        internal/integration/harness_test.go agent/upgrade/upgrade.go
git commit -m "feat(hub): upgrade trigger at handshake and on-demand, result handler"
```

---

### Task 14: Server CLI sign and release ingest

**Files:**
- Create: `cmd/kumaboard/sign.go`
- Create: `cmd/kumaboard/release.go`
- Modify: `cmd/kumaboard/main.go`

**Step 1: Write the sign subcommand**

```go
// cmd/kumaboard/sign.go
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jhyoong/KumaBoard/internal/signing"
)

type manifest struct {
	Version   string     `json:"version"`
	Artifacts []artifact `json:"artifacts"`
}

type artifact struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	Signature string `json:"signature"`
}

func runSign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to config.yaml")
	version := fs.String("version", "", "release version to sign")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *version == "" {
		return fmt.Errorf("usage: kumaboard sign --version x.y.z [-config PATH]")
	}
	cfg, err := loadConfigFlag([]string{"-config", *cfgPath})
	if err != nil {
		return err
	}
	keyPath := filepath.Join(cfg.DataDir, "pki", "release_ed25519")
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("read signing key %s: %w (must run as root)", keyPath, err)
	}
	if len(keyBytes) != ed25519.PrivateKeySize {
		return fmt.Errorf("signing key is %d bytes, want %d", len(keyBytes), ed25519.PrivateKeySize)
	}
	priv := ed25519.PrivateKey(keyBytes)

	relDir := filepath.Join(cfg.DataDir, "releases", *version)
	mfPath := filepath.Join(relDir, "manifest.json")
	mfBytes, err := os.ReadFile(mfPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var mf manifest
	if err := json.Unmarshal(mfBytes, &mf); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if mf.Version != *version {
		return fmt.Errorf("manifest version %q does not match --version %q", mf.Version, *version)
	}
	for i := range mf.Artifacts {
		a := &mf.Artifacts[i]
		binName := "kuma-agent"
		if a.OS == "windows" {
			binName = "kuma-agent.exe"
		}
		binPath := filepath.Join(relDir, a.OS+"_"+a.Arch, binName)
		hash, err := fileSHA256(binPath)
		if err != nil {
			return fmt.Errorf("hash %s: %w", binPath, err)
		}
		if hash != a.SHA256 {
			return fmt.Errorf("sha256 mismatch for %s_%s: manifest %s, computed %s", a.OS, a.Arch, a.SHA256, hash)
		}
		msg := signing.BuildMessage(*version, a.OS, a.Arch, hash)
		sig := signing.Sign(priv, msg)
		a.Signature = base64.StdEncoding.EncodeToString(sig)
		fmt.Printf("signed %s_%s\n", a.OS, a.Arch)
	}
	out, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(mfPath, out, 0o644); err != nil {
		return err
	}
	fmt.Printf("manifest updated: %s\n", mfPath)
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
```

**Step 2: Write the release ingest subcommand**

```go
// cmd/kumaboard/release.go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jhyoong/KumaBoard/server/store"
)

func runReleaseIngest(args []string) error {
	fs := flag.NewFlagSet("release ingest", flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to config.yaml")
	version := fs.String("version", "", "release version to ingest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *version == "" {
		return fmt.Errorf("usage: kumaboard release ingest --version x.y.z [-config PATH]")
	}
	cfg, err := loadConfigFlag([]string{"-config", *cfgPath})
	if err != nil {
		return err
	}
	mfPath := filepath.Join(cfg.DataDir, "releases", *version, "manifest.json")
	b, err := os.ReadFile(mfPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var mf manifest
	if err := json.Unmarshal(b, &mf); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	for _, a := range mf.Artifacts {
		if a.Signature == "" {
			return fmt.Errorf("artifact %s_%s has no signature; run kumaboard sign first", a.OS, a.Arch)
		}
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "kumaboard.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	for _, a := range mf.Artifacts {
		if err := st.IngestRelease(ctx, mf.Version, a.OS, a.Arch, a.SHA256, a.Signature, a.SizeBytes); err != nil {
			return fmt.Errorf("ingest %s_%s: %w", a.OS, a.Arch, err)
		}
		st.Audit(ctx, "cli", "release_ingest", mf.Version, "ok", a.OS+"_"+a.Arch)
		fmt.Printf("ingested %s %s_%s\n", mf.Version, a.OS, a.Arch)
	}
	return nil
}
```

**Step 3: Wire into main.go**

Add to `usage()`:

```
  sign           sign release artifacts (flags: --version, -config PATH)
  release ingest ingest signed release into database (flags: --version, -config PATH)
```

Add to the switch:

```go
	case "sign":
		err = runSign(os.Args[2:])
	case "release":
		if len(os.Args) < 3 || os.Args[2] != "ingest" {
			usage()
		}
		err = runReleaseIngest(os.Args[3:])
```

**Step 4: Verify it compiles**

Run: `cd /path/to/KumaBoard && go build ./cmd/kumaboard/`
Expected: no errors

**Step 5: Commit**

```bash
git add cmd/kumaboard/sign.go cmd/kumaboard/release.go cmd/kumaboard/main.go
git commit -m "feat(cli): kumaboard sign and release ingest subcommands"
```

---

### Task 16: Dashboard UpgradePanel component

**Files:**
- Create: `web/src/components/UpgradePanel.tsx`
- Modify: `web/src/views/DeviceDetail.tsx`
- Modify: `web/src/api.ts`

**Step 1: Add types to api.ts**

```typescript
// Add to api.ts:

export interface UpgradeEntry {
  id: string;
  device_id: number;
  from_version: string;
  to_version: string;
  requested_by: string;
  started_at: string;
  finished_at: string | null;
  state: string;
  failure_reason: string;
}

export interface ReleaseEntry {
  version: string;
  os: string;
  arch: string;
  added_at: string;
}
```

**Step 2: Write the UpgradePanel component**

```tsx
// web/src/components/UpgradePanel.tsx
import { useEffect, useState } from 'react'
import { api, type Device, type UpgradeEntry, type ReleaseEntry } from '../api'
import { when } from '../format'

const IN_FLIGHT = new Set(['requested', 'downloading', 'verifying', 'selftest', 'swapped', 'restarting'])
const FAILED = new Set(['rolled_back', 'failed'])

export function UpgradePanel({ device }: { device: Device }) {
  const [upgrades, setUpgrades] = useState<UpgradeEntry[]>([])
  const [releases, setReleases] = useState<ReleaseEntry[]>([])
  const [target, setTarget] = useState(device.desired_agent_version || '')
  const [msg, setMsg] = useState('')

  useEffect(() => {
    api<UpgradeEntry[]>(`/api/devices/${device.name}/upgrades`).then(setUpgrades).catch(() => {})
    if (device.os && device.arch) {
      api<ReleaseEntry[]>(`/api/releases?os=${device.os}&arch=${device.arch}`).then(setReleases).catch(() => {})
    }
  }, [device.name, device.os, device.arch])

  useEffect(() => {
    setTarget(device.desired_agent_version || '')
  }, [device.desired_agent_version])

  const latest = upgrades[0]
  const isInFlight = latest && IN_FLIGHT.has(latest.state)
  const isFailed = latest && FAILED.has(latest.state)

  const setVersion = async (v: string) => {
    try {
      await api(`/api/devices/${device.name}`, {
        method: 'PATCH',
        body: JSON.stringify({ desired_agent_version: v }),
      })
      setTarget(v)
      setMsg(v ? `Target set to ${v}` : 'Target cleared')
    } catch (e) {
      setMsg((e as Error).message)
    }
  }

  const retry = async () => {
    try {
      await api(`/api/devices/${device.name}/upgrades/retry`, { method: 'POST' })
      setMsg('Retry requested')
    } catch (e) {
      setMsg((e as Error).message)
    }
  }

  const abandon = async () => {
    if (!latest || !confirm('Abandon this upgrade?')) return
    try {
      await api(`/api/devices/${device.name}/upgrades/${latest.id}/abandon`, { method: 'POST' })
      setMsg('Abandoned')
    } catch (e) {
      setMsg((e as Error).message)
    }
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center gap-3">
        <label className="text-sm font-medium">Target version</label>
        <select
          className="rounded border px-2 py-1 text-sm"
          value={target}
          onChange={(e) => setVersion(e.target.value)}
        >
          <option value="">-- none --</option>
          {releases.map((r) => (
            <option key={r.version} value={r.version}>{r.version}</option>
          ))}
        </select>
      </div>

      {latest && (
        <div className="rounded border p-3 text-sm">
          <div className="flex items-center gap-2">
            <span className="font-medium">Latest upgrade:</span>
            <span>{latest.from_version} &rarr; {latest.to_version}</span>
            <span className={`rounded px-2 py-0.5 text-xs font-medium ${
              latest.state === 'verified' ? 'bg-green-100 text-green-700' :
              isFailed ? 'bg-red-100 text-red-700' :
              'bg-yellow-100 text-yellow-700'
            }`}>{latest.state}</span>
          </div>
          {latest.failure_reason && (
            <div className="mt-1 text-gray-500">Reason: {latest.failure_reason}</div>
          )}
          <div className="mt-1 text-gray-400">Started {when(latest.started_at)}</div>
          <div className="mt-2 flex gap-2">
            {isFailed && <button className="rounded bg-blue-600 px-3 py-1 text-xs text-white" onClick={retry}>Retry</button>}
            {isInFlight && <button className="rounded bg-red-600 px-3 py-1 text-xs text-white" onClick={abandon}>Abandon</button>}
          </div>
        </div>
      )}

      {msg && <div className="text-sm text-gray-600">{msg}</div>}
    </div>
  )
}
```

**Step 3: Mount in DeviceDetail**

In `web/src/views/DeviceDetail.tsx`, import and add the panel:

```tsx
import { UpgradePanel } from '../components/UpgradePanel'

// After the Commands section and before Run history:
      <section>
        <h2 className="mb-2 font-medium">Agent upgrade</h2>
        <UpgradePanel device={device} />
      </section>
```

**Step 4: Verify it compiles**

Run: `cd /path/to/KumaBoard/web && npm run build`
Expected: no errors

**Step 5: Commit**

```bash
git add web/src/components/UpgradePanel.tsx web/src/views/DeviceDetail.tsx web/src/api.ts
git commit -m "feat(web): upgrade panel on device detail page"
```

---

### Task 17: Integration tests

**Files:**
- Create: `internal/integration/upgrade_test.go`
- Modify: `internal/integration/harness_test.go` (minor additions)

**Step 1: Write upgrade integration tests**

Tests to cover:

- `TestUpgradeTriggerOnHandshake`: set desired version, connect agent, verify
  `upgrade_request` is sent
- `TestNoUpgradeWhenVersionsMatch`: agent version matches desired, no request sent
- `TestNoUpgradeWhenAnotherInFlight`: one upgrade active, second device not triggered
- `TestNoUpgradeAfterFailed`: failed row blocks re-send
- `TestUpgradeResultUpdatesRow`: agent sends `upgrade_result`, row state updates
- `TestRollbackDetection`: agent reconnects at `from_version` while row is in-flight

These tests use the existing harness pattern. They need the store to have
releases and desired versions set up, and they observe what messages the hub
sends and what store state changes.

The tests are lengthy; the implementation agent should write them following the
patterns in `connection_test.go` and `commands_test.go`. Key setup for each:

1. Create device, ingest a release
2. Set `desired_agent_version`
3. Connect an agent (or use a mock that captures sent envelopes)
4. Assert store state and messages

**Step 2: Run tests**

Run: `cd /path/to/KumaBoard && go test ./internal/integration/ -v -run TestUpgrade`
Expected: all PASS

**Step 3: Run full test suite**

Run: `cd /path/to/KumaBoard && go test ./...`
Expected: all PASS

**Step 4: Commit**

```bash
git add internal/integration/upgrade_test.go internal/integration/harness_test.go
git commit -m "test: integration tests for upgrade trigger, result, and rollback"
```

---

## Build order summary

| Task | Depends on | What it delivers |
|---|---|---|
| 9. store upgrades | nothing | CRUD for upgrades and releases |
| 10. device token auth | nothing | Middleware for artifact downloads |
| 11. artifact serving | 9, 10 | Serve binaries, list releases |
| 12. upgrade API | 9, 11 | Retry, abandon, desired version |
| 13. hub upgrade logic | 9, 12 | Trigger model, result handler |
| 14. CLI sign + ingest | 2A-1 (signing), 9 | kumaboard sign/release ingest |
| 16. dashboard | 11, 12 | UpgradePanel component |
| 17. integration tests | all above + 2A | End-to-end verification |

Tasks 9 and 10 have no dependencies on each other or on Part 2A and can be
parallelized.
