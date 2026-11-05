package product

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/7ar-chat/harness"
)

// The user's document library (a CV, templates...): files every conversation of the user, and the agents in it, can use.
//
//	GET    /api/me/documents          → [{name, size, mimeType, modified}]
//	PUT    /api/me/documents/{name}   <file bytes> → the document   (at most 20 MiB, 50 files)
//	GET    /api/me/documents/{name}   → the file (as a download)
//	DELETE /api/me/documents/{name}   → 204
//
// Without HARNESS_DOCUMENTS_DIR the harness has no library and these answer 404.

func (s *server) routeDocuments() {
	s.Route("GET /me/documents", s.listDocuments)
	s.Route("PUT /me/documents/{name}", s.putDocument)
	s.Route("GET /me/documents/{name}", s.getDocument)
	s.Route("DELETE /me/documents/{name}", s.deleteDocument)
}

// hasLibrary answers 404 and reports false when the server keeps no document library.
func (s *server) hasLibrary(w http.ResponseWriter) bool {
	if s.H == nil || s.H.Documents == nil {
		writeError(w, http.StatusNotFound, "not_found", "This server keeps no document library.")
		return false
	}
	return true
}

func (s *server) listDocuments(w http.ResponseWriter, _ *http.Request, c Caller) {
	if !s.hasLibrary(w) {
		return
	}
	lib := s.H.Documents
	docs, err := lib.List(c.Actor)
	if err != nil {
		fail(w, err)
		return
	}
	if docs == nil {
		docs = []harness.Document{}
	}
	writeJSON(w, http.StatusOK, docs)
}

func (s *server) putDocument(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.hasLibrary(w) {
		return
	}
	lib := s.H.Documents
	r.Body = http.MaxBytesReader(w, r.Body, harness.MaxDocumentBytes+1)
	data, err := io.ReadAll(r.Body)
	if err != nil || len(data) > harness.MaxDocumentBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "A document must be at most "+strconv.Itoa(harness.MaxDocumentBytes>>20)+" MiB.")
		return
	}
	d, err := lib.Put(c.Actor, r.PathValue("name"), data)
	switch {
	case errors.Is(err, harness.ErrDocumentInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, harness.ErrDocumentLimit):
		writeError(w, http.StatusConflict, "limit_reached", err.Error())
	case err != nil:
		fail(w, err)
	default:
		writeJSON(w, http.StatusOK, d)
	}
}

func (s *server) getDocument(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.hasLibrary(w) {
		return
	}
	lib := s.H.Documents
	data, d, err := lib.Get(c.Actor, r.PathValue("name"))
	if errors.Is(err, harness.ErrDocumentNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "No such document.")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", d.MimeType)
	h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(d.Name))
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, no-store")
	_, _ = w.Write(data)
}

func (s *server) deleteDocument(w http.ResponseWriter, r *http.Request, c Caller) {
	if !s.hasLibrary(w) {
		return
	}
	lib := s.H.Documents
	if err := lib.Delete(c.Actor, r.PathValue("name")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
