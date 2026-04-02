package dashboard

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Handler serves the React SPA from embedded files.
// Static files (js, css, images) are served directly.
// All other paths fall back to index.html for client-side routing.
func Handler() http.Handler {
	dist, _ := fs.Sub(distFS, "dist")
	fileServer := http.FileServer(http.FS(dist))
	indexHTML, _ := fs.ReadFile(dist, "index.html")

	return http.StripPrefix("/dashboard", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		urlPath := r.URL.Path

		// Normalize: strip trailing slashes to prevent redirect loops
		if urlPath != "/" && strings.HasSuffix(urlPath, "/") {
			urlPath = strings.TrimRight(urlPath, "/")
			r.URL.Path = urlPath
		}

		if urlPath == "" || urlPath == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(indexHTML)
			return
		}

		// Check if the file exists in the embedded filesystem
		cleanPath := strings.TrimPrefix(urlPath, "/")
		if _, err := fs.Stat(dist, cleanPath); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}

		// If path has a file extension, it's a missing asset — return 404
		if path.Ext(urlPath) != "" {
			http.NotFound(w, r)
			return
		}

		// SPA fallback: serve index.html directly for client-side routes
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	}))
}
