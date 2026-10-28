package httpserver

import (
	"errors"
	"mime"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GhaziBenDahmane/harness/sandbox"
)

// maxDownload is the largest file the download route serves.
const maxDownload = 100 << 20

type fileDTO struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Type       string    `json:"type"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

// confine resolves a request path inside /workspace: "" is the workspace itself,
// a relative path is taken under it, and anything that escapes it is refused.
func confine(p string) (string, bool) {
	if strings.ContainsRune(p, 0) {
		return "", false
	}
	if p == "" {
		return sandbox.Workspace, true
	}
	if !strings.HasPrefix(p, "/") {
		p = sandbox.Workspace + "/" + p
	}
	// Clean on the rooted path resolves every "..", so the prefix test below is final.
	p = path.Clean(p)
	if p != sandbox.Workspace && !strings.HasPrefix(p, sandbox.Workspace+"/") {
		return "", false
	}
	return p, true
}

// sandboxOf returns the thread's sandbox, or nil with the response written.
// empty is true when the thread has no sandbox yet (nothing to list).
func (s *server) sandboxOf(w http.ResponseWriter, r *http.Request, c Caller) (sb sandbox.Sandbox, empty, ok bool) {
	if !s.ownsThread(w, r, c) {
		return nil, false, false
	}
	if s.h.Sandboxes == nil || !s.h.Sandboxes.Enabled() {
		writeError(w, http.StatusNotImplemented, "no_sandbox", "This server has no sandbox.")
		return nil, false, false
	}
	sb, err := s.h.Sandboxes.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, sandbox.ErrNoSandbox) || errors.Is(err, sandbox.ErrNotFound) {
		return nil, true, true
	}
	if err != nil {
		fail(w, err)
		return nil, false, false
	}
	return sb, false, true
}

func (s *server) listFiles(w http.ResponseWriter, r *http.Request, c Caller) {
	dir, ok := confine(r.URL.Query().Get("path"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_path", "The path must be inside /workspace.")
		return
	}
	sb, empty, ok := s.sandboxOf(w, r, c)
	if !ok {
		return
	}
	out := []fileDTO{}
	if !empty {
		infos, err := sb.List(r.Context(), dir, 1)
		if errors.Is(err, sandbox.ErrNotFound) {
			infos, err = nil, nil
		}
		if err != nil {
			fail(w, err)
			return
		}
		for _, f := range infos {
			p, ok := confine(f.Path)
			if !ok || p == dir {
				continue
			}
			out = append(out, fileDTO{Name: path.Base(p), Path: p, Type: f.Type, Size: f.Size, ModifiedAt: f.ModifiedAt})
		}
		sort.Slice(out, func(i, j int) bool {
			if (out[i].Type == "directory") != (out[j].Type == "directory") {
				return out[i].Type == "directory"
			}
			return out[i].Name < out[j].Name
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) downloadFile(w http.ResponseWriter, r *http.Request, c Caller) {
	p, ok := confine(r.URL.Query().Get("path"))
	if !ok || p == sandbox.Workspace {
		writeError(w, http.StatusBadRequest, "invalid_path", "The path must be a file inside /workspace.")
		return
	}
	sb, empty, ok := s.sandboxOf(w, r, c)
	if !ok {
		return
	}
	if empty {
		writeError(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	info, err := sb.Stat(r.Context(), p)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "Not found.")
			return
		}
		fail(w, err)
		return
	}
	if info.Type == "directory" {
		writeError(w, http.StatusBadRequest, "invalid_path", "A directory cannot be downloaded.")
		return
	}
	if info.Size > maxDownload {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "The file is too large to download.")
		return
	}
	data, _, err := sb.ReadFile(r.Context(), p, maxDownload)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "Not found.")
			return
		}
		fail(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", dispositionFor(path.Base(p)))
	h.Set("Content-Length", strconv.Itoa(len(data)))
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
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
