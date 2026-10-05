package api

import (
	"log/slog"
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

type server struct {
	Deps
}

// New builds the API handler tree.
func New(d Deps) http.Handler {
	s := &server{Deps: d}
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
	authed.Handle("GET /api/events", auth.OriginCheck(d.AllowedHosts)(d.Broker))
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
	return root
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
