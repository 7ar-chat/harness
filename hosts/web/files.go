package httpserver

import (
	"mime"
	"net/http"
	"strconv"
	"strings"
)

func (s *server) listFiles(w http.ResponseWriter, r *http.Request, c Caller) {
	files, err := s.api(c).ListFiles(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, files)
}

func (s *server) downloadFile(w http.ResponseWriter, r *http.Request, c Caller) {
	f, err := s.api(c).DownloadFile(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		fail(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", dispositionFor(f.Name))
	h.Set("Content-Length", strconv.Itoa(len(f.Data)))
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(f.Data)
}

// dispositionFor builds an attachment header whose filename cannot break out of
// the header (quotes, control characters, path separators are replaced).
func dispositionFor(name string) string {
	clean := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' || r == '/' || r == ';' {
			return '_'
		}
		return r
	}, name)
	if clean == "" || clean == "." || clean == ".." {
		clean = "download"
	}
	if v := mime.FormatMediaType("attachment", map[string]string{"filename": clean}); v != "" {
		return v
	}
	return "attachment"
}
