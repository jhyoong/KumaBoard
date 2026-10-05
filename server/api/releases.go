package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jhyoong/KumaBoard/server/store"
)

func (s *server) mountReleases(authed *http.ServeMux) {
	authed.HandleFunc("GET /api/releases", s.listReleases)
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
