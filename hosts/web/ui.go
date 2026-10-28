package httpserver

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed ui
var uiFiles embed.FS

// UI is the embedded browser client (plain HTML, CSS and JavaScript, no build step).
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
		http.ServeFileFS(w, r, UI(), "index.html")
	})
}
