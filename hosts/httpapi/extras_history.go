package httpapi

import (
	"net/http"

	"github.com/7ar-chat/harness/hosts/client"
)

// routeHistory registers the history operations of client.Extras.
func (s *Server) routeHistory() {
	s.Route("POST /threads/{id}/compact", s.compact)
	s.Route("POST /threads/{id}/undo", s.undo)
	s.Route("POST /threads/{id}/redo", s.redo)
	s.Route("POST /threads/{id}/fork", s.fork)
}

// extras is the caller's client as client.Extras.
func (s *Server) extras(c Caller) client.Extras { return s.API(c).(client.Extras) }

func (s *Server) compact(w http.ResponseWriter, r *http.Request, c Caller) {
	res, err := s.extras(c).Compact(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) undo(w http.ResponseWriter, r *http.Request, c Caller) {
	res, err := s.extras(c).Undo(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) redo(w http.ResponseWriter, r *http.Request, c Caller) {
	res, err := s.extras(c).Redo(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) fork(w http.ResponseWriter, r *http.Request, c Caller) {
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
	if s.policy.AfterFork(w, r, c, th) {
		return
	}
	writeJSON(w, http.StatusCreated, th)
}
