package httpserver

import (
	"net/http"

	"github.com/GhaziBenDahmane/harness/hosts/client"
)

// routeHistory registers the history operations of client.Extras.
func (s *server) routeHistory() {
	s.route("POST /threads/{id}/compact", s.compact)
	s.route("POST /threads/{id}/undo", s.undo)
	s.route("POST /threads/{id}/redo", s.redo)
	s.route("POST /threads/{id}/fork", s.fork)
}

// extras is the caller's client as client.Extras.
func (s *server) extras(c Caller) client.Extras { return s.api(c).(client.Extras) }

func (s *server) compact(w http.ResponseWriter, r *http.Request, c Caller) {
	res, err := s.extras(c).Compact(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) undo(w http.ResponseWriter, r *http.Request, c Caller) {
	res, err := s.extras(c).Undo(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) redo(w http.ResponseWriter, r *http.Request, c Caller) {
	res, err := s.extras(c).Redo(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) fork(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		MessageID string `json:"messageId"`
	}
	if !decode(w, r, &body) {
		return
	}
	th, err := s.extras(c).Fork(r.Context(), r.PathValue("id"), body.MessageID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, th)
}
