package api

import (
	"log/slog"
	"mime"
	"net"
	"net/http"

	"github.com/jhyoong/KumaBoard/server/auth"
	"github.com/jhyoong/KumaBoard/server/hub"
	"github.com/jhyoong/KumaBoard/server/registry"
	"github.com/jhyoong/KumaBoard/server/sse"
	"github.com/jhyoong/KumaBoard/server/store"
	"github.com/jhyoong/KumaBoard/server/terminal"
)

// WakeFunc sends a Wake-on-LAN magic packet to the given MAC address.
type WakeFunc func(mac string) error

// Deps holds all dependencies for the API server.
type Deps struct {
	Store        *store.Store
	Registry     *registry.Registry
	Hub          *hub.Hub
	Broker       *sse.Broker
	Sessions     *auth.Sessions
	Limiter      *auth.Limiter
	Terminal     *terminal.Broker
	AllowedHosts map[string]bool
	Log          *slog.Logger
	Wake         WakeFunc
	ReleasesDir  string
}

// maxPasswordHashes bounds concurrent Argon2id verifications. Each one
// allocates 64 MiB, and /api/login is reachable without credentials.
const maxPasswordHashes = 2

type server struct {
	Deps
	hashSlots chan struct{}
}

// New builds the API handler tree.
func New(d Deps) http.Handler {
	s := &server{Deps: d, hashSlots: make(chan struct{}, maxPasswordHashes)}
	authed := http.NewServeMux()
	authed.HandleFunc("POST /api/logout", s.logout)
	authed.HandleFunc("GET /api/me", s.me)
	authed.HandleFunc("GET /api/devices", s.listDevices)
	authed.HandleFunc("POST /api/devices", s.registerDevice)
	authed.HandleFunc("PATCH /api/devices/{name}", s.updateDevice)
	authed.HandleFunc("POST /api/devices/{name}/revoke", s.revokeDevice)
	authed.HandleFunc("GET /api/devices/{name}/metrics", s.deviceMetrics)
	authed.HandleFunc("GET /api/devices/{name}/metrics/history", s.deviceMetricsHistory)
	authed.HandleFunc("GET /api/devices/{name}/commands", s.deviceCommands)
	authed.HandleFunc("GET /api/audit", s.listAudit)
	authed.Handle("GET /api/events", d.Broker)
	s.mountRuns(authed)
	s.mountWake(authed)
	s.mountReleases(authed)
	s.mountUpgrades(authed)
	authed.HandleFunc("POST /api/devices/{name}/terminal", s.requestTerminal)

	agentMux := http.NewServeMux()
	agentMux.HandleFunc("GET /api/agent/releases/{version}/{os_arch}", s.serveArtifact)

	root := http.NewServeMux()
	root.HandleFunc("POST /api/login", s.login)
	root.Handle("/api/agent/", auth.RequireDeviceToken(d.Store)(agentMux))
	root.Handle("/api/", auth.RequireSession(d.Sessions)(authed))
	// The session cookie is SameSite=Strict, but "site" ignores the port, so
	// another web UI on this host could otherwise drive the API. Checking
	// Origin, and requiring a content type a cross-origin form cannot send,
	// closes that. Agents send no Origin and only GET.
	return auth.OriginCheck(d.AllowedHosts)(requireJSON(root))
}

// requireJSON rejects state-changing requests that are not declared as JSON.
func requireJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
