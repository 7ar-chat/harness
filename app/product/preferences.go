package product

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

// A person's own small settings document, so what the interface remembers follows them across browsers: where they are in
// the getting-started steps, which explorer sections are folded, the panel sizes. Only the caller's own document is
// reachable (there is no login in the path), it is at most store.MaxPreferencesBytes of JSON, and any member may use it.
//
//	GET   /api/me/preferences                  -> {values: {key: json}, updatedAt}
//	PATCH /api/me/preferences {values: {...}}  -> the new document ("key": null removes a key)

// preferencesView is the document as the API shows it.
type preferencesView struct {
	Values map[string]json.RawMessage `json:"values"`
	// UpdatedAt is when the document last changed; absent when it never did.
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

// preferencesPatch is the body of PATCH /me/preferences.
type preferencesPatch struct {
	Values map[string]json.RawMessage `json:"values"`
}

func viewOfPreferences(p store.UserPreferences) preferencesView {
	v := preferencesView{Values: p.Values}
	if v.Values == nil {
		v.Values = map[string]json.RawMessage{}
	}
	if !p.UpdatedAt.IsZero() && p.UpdatedAt.Year() > 1970 {
		t := p.UpdatedAt
		v.UpdatedAt = &t
	}
	return v
}

func (s *server) routePreferences() {
	s.Route("GET /me/preferences", s.getPreferences)
	s.Route("PATCH /me/preferences", s.patchPreferences)
}

func (s *server) getPreferences(w http.ResponseWriter, r *http.Request, c Caller) {
	p, err := s.team.Preferences(r.Context(), c.Actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, viewOfPreferences(p))
}

func (s *server) patchPreferences(w http.ResponseWriter, r *http.Request, c Caller) {
	var body preferencesPatch
	if !decode(w, r, &body) {
		return
	}
	p, err := s.team.PatchPreferences(r.Context(), c.Actor, body.Values)
	switch {
	case errors.Is(err, store.ErrPreferencesTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "Preferences are at most 8 KB in all.")
	case errors.Is(err, store.ErrPreferencesKey):
		writeError(w, http.StatusBadRequest, "invalid_request", "A preference key is lower case letters, digits, dots, dashes and underscores (at most 64), and its value must be JSON.")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "Your account is not registered yet; log in again.")
	case err != nil:
		fail(w, err)
	default:
		writeJSON(w, http.StatusOK, viewOfPreferences(p))
	}
}
