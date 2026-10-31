package product

import (
	"net/http"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

// Unread state is kept per user on the server, so it follows the user across
// browsers and replicas: the last time each thread was seen. The UI compares it
// with the thread's updatedAt (a thread is unread when it changed after the last
// visit; one never visited counts as seen).

func (s *server) listSeen(w http.ResponseWriter, r *http.Request, c Caller) {
	seen, err := s.team.SeenThreads(r.Context(), c.Actor)
	if err != nil {
		fail(w, err)
		return
	}
	out := make(map[string]time.Time, len(seen))
	for id, at := range seen {
		out[id] = at
	}
	writeJSON(w, http.StatusOK, out)
}

// markSeen records that the caller saw a thread up to "at" (default now). Any member, viewers included,
// may do it: it concerns the caller only. A thread the caller cannot see is a 404.
func (s *server) markSeen(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		ThreadID string    `json:"threadId"`
		At       time.Time `json:"at"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.ThreadID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "threadId is required.")
		return
	}
	if _, err := s.access(r.Context(), c.Actor, body.ThreadID, store.RoleViewer); err != nil {
		s.FailAccess(w, err)
		return
	}
	if body.At.IsZero() || body.At.After(time.Now().Add(time.Minute)) {
		body.At = time.Now()
	}
	if err := s.team.SetThreadSeen(r.Context(), c.Actor, body.ThreadID, body.At); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
