package httpapi

import (
	"net/http"

	"github.com/7ar-chat/harness/hosts/client"
)

// routeCommands registers the custom command routes:
//
//	GET  /api/commands                       → [Command]
//	POST /api/commands/{name}/expand {"args"} → {"prompt"}
func (s *Server) routeCommands() {
	s.Route("GET /commands", s.listCommands)
	s.Route("POST /commands/{name}/expand", s.expandCommand)
}

func (s *Server) listCommands(w http.ResponseWriter, r *http.Request, c Caller) {
	ex, ok := s.API(c).(client.Extras)
	if !ok {
		fail(w, client.ErrNotSupported)
		return
	}
	list, err := ex.Commands(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	if list == nil {
		list = []client.Command{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) expandCommand(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		Args string `json:"args"`
	}
	if !decode(w, r, &body) {
		return
	}
	ex, ok := s.API(c).(client.Extras)
	if !ok {
		fail(w, client.ErrNotSupported)
		return
	}
	prompt, err := ex.ExpandCommand(r.Context(), r.PathValue("name"), body.Args)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"prompt": prompt})
}
