package api

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/jhyoong/KumaBoard/web"
)

// Static returns an http.Handler that serves the embedded frontend.
// Known files are served directly; unknown paths fall back to index.html
// for client-side routing.
func Static() http.Handler {
	dist, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		panic(err)
	}
	files := http.FS(dist)
	fileServer := http.FileServer(files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean(r.URL.Path)
		if p != "/" {
			if f, err := files.Open(p); err == nil {
				f.Close()
				if strings.HasPrefix(p, "/assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}
