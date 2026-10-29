package httpserver

import (
	"net/http"
	"strings"

	"github.com/GhaziBenDahmane/harness/hosts/client"
	"github.com/GhaziBenDahmane/harness/store"
)

func (s *server) routeProjectExtras() {}

func (s *server) createThreadTeam(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		Title     string `json:"title"`
		ProjectID string `json:"projectId"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.ProjectID != "" {
		if _, err := s.requireProject(r.Context(), body.ProjectID, c.Actor, store.RoleMember); err != nil {
			s.failAccess(w, err)
			return
		}
	}
	th, err := s.api(c).CreateThread(r.Context(), body.Title)
	if err != nil {
		fail(w, err)
		return
	}
	meta := store.ThreadMeta{ThreadID: th.ID, ProjectID: body.ProjectID, Owner: c.Actor, CreatedBy: c.Actor, Kind: store.ThreadChat}
	if err := s.team.SetThreadMeta(r.Context(), meta); err != nil {
		_, _ = s.h.Store.DeleteThread(r.Context(), c.Actor, th.ID)
		fail(w, err)
		return
	}
	s.note(Caller{Login: c.Actor, Actor: c.Actor, ProjectID: body.ProjectID}, th.ID, "thread.create", nil, FeedEvent{Type: "thread.created", Title: th.Title})
	writeJSON(w, http.StatusCreated, threadView{Thread: *th, ProjectID: body.ProjectID, CreatedBy: c.Actor, Kind: meta.Kind, Status: "idle"})
}

func (s *server) listThreadsTeam(w http.ResponseWriter, r *http.Request, c Caller) {
	if pid := r.URL.Query().Get("project"); pid != "" {
		if _, err := s.requireProject(r.Context(), pid, c.Actor, store.RoleViewer); err != nil {
			s.failAccess(w, err)
			return
		}
		out, err := s.projectThreads(r.Context(), pid)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, nonNil(out))
		return
	}
	out, err := s.personalThreads(r.Context(), c.Actor, limitParam(r, 50))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(out))
}

func (s *server) getThreadTeam(w http.ResponseWriter, r *http.Request, c Caller) {
	th, err := s.api(c).GetThread(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	s.withAuthors(r.Context(), th)
	v := threadView{Thread: *th, ProjectID: c.ProjectID, CreatedBy: c.CreatedBy, Role: c.Role, Kind: store.ThreadChat}
	if meta, err := s.team.GetThreadMeta(r.Context(), th.ID); err == nil {
		v.Kind = meta.Kind
	}
	switch {
	case th.PendingApproval != nil:
		v.Status = "waiting_approval"
	case th.PendingQuestion != nil:
		v.Status = "waiting_input"
	case th.ActiveRun != nil:
		v.Status = th.ActiveRun.Status
	default:
		v.Status = "idle"
	}
	writeJSON(w, http.StatusOK, v)
}

// canDeleteThread: the thread's creator, or an owner of its project.
func canDeleteThread(c Caller) bool {
	return c.ProjectID == "" || c.CreatedBy == c.Actor || c.Role == store.RoleOwner
}

func (s *server) deleteThreadTeam(w http.ResponseWriter, r *http.Request, c Caller) {
	if !canDeleteThread(c) {
		s.failAccess(w, errForbidden)
		return
	}
	id := r.PathValue("id")
	if err := s.api(c).DeleteThread(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	_ = s.team.DeleteThreadMeta(r.Context(), id)
	s.note(c, id, "thread.delete", nil, FeedEvent{Type: "thread.deleted"})
	w.WriteHeader(http.StatusNoContent)
}

// afterRun records a run started by the actor and follows it.
func (s *server) afterRun(c Caller, run *client.Run, action string, detail map[string]any) {
	if s.team == nil || run == nil {
		return
	}
	s.note(c, run.ThreadID, action, detail, FeedEvent{Type: "run.started", RunID: run.ID})
	s.follow(c, run)
}

// pendingToolName is the tool the thread is waiting for a decision on ("" when none).
func (s *server) pendingToolName(r *http.Request, c Caller, threadID string) string {
	t, err := s.h.Store.GetThread(r.Context(), c.Login, threadID)
	if err != nil || t.PendingApproval == nil {
		return ""
	}
	return t.PendingApproval.ToolName
}

func trimTitle(s string) string { return strings.TrimSpace(s) }
