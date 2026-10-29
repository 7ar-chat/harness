package httpapi

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:ui
var uiFiles embed.FS

// UI is the embedded browser client: the build of web/ (make web). A checkout
// that has not built it embeds only a placeholder and serves a notice at /.
func UI() fs.FS {
	sub, _ := fs.Sub(uiFiles, "ui")
	return sub
}

// mountUI serves the UI at / (index.html) and /ui/….
func mountUI(mux *http.ServeMux) {
	files := http.FileServerFS(UI())
	mux.Handle("GET /ui/", http.StripPrefix("/ui/", files))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		if _, err := fs.Stat(UI(), "index.html"); err != nil {
			http.Error(w, "The web UI is not built into this binary: run `make web`, then rebuild.", http.StatusNotFound)
			return
		}
		http.ServeFileFS(w, r, UI(), "index.html")
	})
}
