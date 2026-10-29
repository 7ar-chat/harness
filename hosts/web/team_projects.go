package httpserver

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/GhaziBenDahmane/harness/store"
)

func (s *server) routeTeam() {
	s.route("GET /me", s.me)
	s.routeProfile()
	s.routeConnectors()
	s.route("GET /projects", s.listProjects)
	s.route("POST /projects", s.createProject)
	s.route("GET /projects/{pid}", s.getProject)
	s.route("PATCH /projects/{pid}", s.patchProject)
	s.route("DELETE /projects/{pid}", s.deleteProject)
	s.route("GET /projects/{pid}/members", s.listMembers)
	s.route("PUT /projects/{pid}/members/{login}", s.putMember)
	s.route("DELETE /projects/{pid}/members/{login}", s.removeMember)
	s.route("GET /projects/{pid}/invites", s.listInvites)
	s.route("POST /projects/{pid}/invites", s.createInvite)
	s.route("DELETE /projects/{pid}/invites", s.deleteInvite)
	s.route("GET /projects/{pid}/audit", s.listAudit)
	s.route("GET /overview", s.overview)
	s.route("GET /events", s.feed)
	s.routeGitHub()
}

type projectView struct {
	store.Project
	Role string `json:"role"`
}

func (s *server) me(w http.ResponseWriter, r *http.Request, c Caller) {
	u, err := s.team.GetUser(r.Context(), c.Actor)
	if errors.Is(err, store.ErrNotFound) {
		u = &store.User{Login: c.Actor, Name: c.Actor}
	} else if err != nil {
		fail(w, err)
		return
	}
	list, err := s.team.ListProjects(r.Context(), c.Actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": viewOf(*u), "projects": nonNil(list)})
}

func nonNil[T any](l []T) []T {
	if l == nil {
		return []T{}
	}
	return l
}

func (s *server) listProjects(w http.ResponseWriter, r *http.Request, c Caller) {
	list, err := s.team.ListProjects(r.Context(), c.Actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(list))
}

func validRepoURL(u string) bool {
	if u == "" {
		return true
	}
	p, err := url.Parse(u)
	return err == nil && (p.Scheme == "https" || p.Scheme == "ssh" || p.Scheme == "git") && p.Host != ""
}

func (s *server) createProject(w http.ResponseWriter, r *http.Request, c Caller) {
	var body struct {
		Name          string `json:"name"`
		Description   string `json:"description"`
		RepoURL       string `json:"repoUrl"`
		DefaultBranch string `json:"defaultBranch"`
	}
	if !decode(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || len(body.Name) > 100 {
		writeError(w, http.StatusBadRequest, "invalid_request", "A project name of 1 to 100 characters is required.")
		return
	}
	if !validRepoURL(body.RepoURL) {
		writeError(w, http.StatusBadRequest, "invalid_request", "repoUrl must be an https, ssh or git URL.")
		return
	}
	p, err := s.team.CreateProject(r.Context(), store.Project{Name: body.Name, Description: body.Description,
		RepoURL: body.RepoURL, DefaultBranch: body.DefaultBranch}, c.Actor)
	if err != nil {
		fail(w, err)
		return
	}
	s.note(Caller{Actor: c.Actor, ProjectID: p.ID}, "", "project.create", map[string]any{"name": p.Name}, FeedEvent{Type: "project.changed", affects: c.Actor})
	writeJSON(w, http.StatusCreated, projectView{*p, store.RoleOwner})
}

// project loads the project of the route with the actor's role, answering the error itself.
func (s *server) project(w http.ResponseWriter, r *http.Request, c Caller, need string) (*store.Project, string, bool) {
	id := r.PathValue("pid")
	role, err := s.requireProject(r.Context(), id, c.Actor, need)
	if err != nil {
		s.failAccess(w, err)
		return nil, "", false
	}
	p, err := s.team.GetProject(r.Context(), id)
	if err != nil {
		s.failAccess(w, errNotFound)
		return nil, "", false
	}
	return p, role, true
}

func (s *server) getProject(w http.ResponseWriter, r *http.Request, c Caller) {
	if p, role, ok := s.project(w, r, c, store.RoleViewer); ok {
		writeJSON(w, http.StatusOK, projectView{*p, role})
	}
}

func (s *server) patchProject(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleOwner)
	if !ok {
		return
	}
	var body struct {
		Name          *string `json:"name"`
		Description   *string `json:"description"`
		RepoURL       *string `json:"repoUrl"`
		DefaultBranch *string `json:"defaultBranch"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Name != nil && (strings.TrimSpace(*body.Name) == "" || len(*body.Name) > 100) {
		writeError(w, http.StatusBadRequest, "invalid_request", "A project name of 1 to 100 characters is required.")
		return
	}
	if body.RepoURL != nil && !validRepoURL(*body.RepoURL) {
		writeError(w, http.StatusBadRequest, "invalid_request", "repoUrl must be an https, ssh or git URL.")
		return
	}
	up, err := s.team.UpdateProject(r.Context(), p.ID, store.ProjectPatch{Name: body.Name, Description: body.Description,
		RepoURL: body.RepoURL, DefaultBranch: body.DefaultBranch})
	if err != nil {
		fail(w, err)
		return
	}
	s.note(Caller{Actor: c.Actor, ProjectID: p.ID}, "", "project.update", nil, FeedEvent{Type: "project.changed"})
	writeJSON(w, http.StatusOK, projectView{*up, store.RoleOwner})
}

func (s *server) deleteProject(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleOwner)
	if !ok {
		return
	}
	// The threads are deleted with the project: they are only reachable through it.
	if metas, err := s.team.ListProjectThreads(r.Context(), p.ID); err == nil {
		for _, m := range metas {
			_, _ = s.h.Store.DeleteThread(r.Context(), m.Owner, m.ThreadID)
		}
	}
	members, _ := s.team.ListMembers(r.Context(), p.ID)
	if err := s.team.DeleteProject(r.Context(), p.ID); err != nil {
		fail(w, err)
		return
	}
	for _, m := range members {
		s.hub.publish(FeedEvent{Type: "project.changed", ProjectID: p.ID, Actor: c.Actor, affects: m.Login})
	}
	w.WriteHeader(http.StatusNoContent)
}

type memberView struct {
	Login     string    `json:"login"`
	Name      string    `json:"name,omitempty"`
	AvatarURL string    `json:"avatarUrl,omitempty"`
	Email     string    `json:"email,omitempty"`
	Role      string    `json:"role"`
	AddedAt   time.Time `json:"addedAt"`
}

func (s *server) listMembers(w http.ResponseWriter, r *http.Request, c Caller) {
	p, role, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	members, err := s.team.ListMembers(r.Context(), p.ID)
	if err != nil {
		fail(w, err)
		return
	}
	logins := make([]string, len(members))
	for i, m := range members {
		logins[i] = m.Login
	}
	users, _ := s.team.GetUsers(r.Context(), logins)
	out := make([]memberView, 0, len(members))
	for _, m := range members {
		v := memberView{Login: m.Login, Name: m.Login, Role: m.Role, AddedAt: m.AddedAt}
		if u := users[m.Login]; u != nil {
			v.Name, v.AvatarURL = u.Label(), u.Avatar()
			if role == store.RoleOwner {
				v.Email = u.Email
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) putMember(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleOwner)
	if !ok {
		return
	}
	var body struct {
		Role string `json:"role"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !store.ValidRole(body.Role) {
		writeError(w, http.StatusBadRequest, "invalid_request", "role must be owner, member or viewer.")
		return
	}
	login := r.PathValue("login")
	if _, err := s.team.GetUser(r.Context(), login); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "No such user: they must log in once, or be invited by email.")
			return
		}
		fail(w, err)
		return
	}
	if err := s.team.SetMember(r.Context(), p.ID, login, body.Role); err != nil {
		s.memberError(w, err)
		return
	}
	s.note(Caller{Actor: c.Actor, ProjectID: p.ID}, "", "member.set", map[string]any{"login": login, "role": body.Role}, FeedEvent{Type: "member.changed", affects: login})
	s.hub.publish(FeedEvent{Type: "member.changed", ProjectID: p.ID, Actor: c.Actor})
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) memberError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrLastOwner) {
		writeError(w, http.StatusConflict, "last_owner", "A project needs at least one owner.")
		return
	}
	s.failAccess(w, err)
}

func (s *server) removeMember(w http.ResponseWriter, r *http.Request, c Caller) {
	id, login := r.PathValue("pid"), r.PathValue("login")
	// Owners remove anyone; a member may leave.
	need := store.RoleOwner
	if login == c.Actor {
		need = store.RoleViewer
	}
	if _, err := s.requireProject(r.Context(), id, c.Actor, need); err != nil {
		s.failAccess(w, err)
		return
	}
	if err := s.team.RemoveMember(r.Context(), id, login); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			err = errNotFound
		}
		s.memberError(w, err)
		return
	}
	s.note(Caller{Actor: c.Actor, ProjectID: id}, "", "member.remove", map[string]any{"login": login}, FeedEvent{Type: "member.changed", affects: login})
	s.hub.publish(FeedEvent{Type: "member.changed", ProjectID: id, Actor: c.Actor})
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) listInvites(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleOwner)
	if !ok {
		return
	}
	list, err := s.team.ListInvites(r.Context(), p.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(list))
}

func (s *server) createInvite(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleOwner)
	if !ok {
		return
	}
	var body struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Role == "" {
		body.Role = store.RoleMember
	}
	if !strings.Contains(body.Email, "@") || !store.ValidRole(body.Role) {
		writeError(w, http.StatusBadRequest, "invalid_request", "A valid email and a role (owner, member, viewer) are required.")
		return
	}
	iv, err := s.team.CreateInvite(r.Context(), store.Invite{ProjectID: p.ID, Email: body.Email, Role: body.Role, InvitedBy: c.Actor})
	if err != nil {
		s.memberError(w, err)
		return
	}
	s.note(Caller{Actor: c.Actor, ProjectID: p.ID}, "", "member.invite", map[string]any{"email": body.Email, "role": body.Role}, FeedEvent{Type: "member.changed"})
	if iv == nil { // the user exists: added at once
		writeJSON(w, http.StatusOK, map[string]any{"added": true})
		return
	}
	writeJSON(w, http.StatusCreated, iv)
}

func (s *server) deleteInvite(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleOwner)
	if !ok {
		return
	}
	if err := s.team.DeleteInvite(r.Context(), p.ID, r.URL.Query().Get("email")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			err = errNotFound
		}
		s.failAccess(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) listAudit(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	list, err := s.team.ListAudit(r.Context(), p.ID, limitParam(r, 100))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(list))
}

// overviewProject is a project with its threads.
type overviewProject struct {
	projectView
	Threads []threadSummary `json:"threads"`
}

// overview lists everything the user can see: personal threads and, per
// project, every member's threads with their status. It is the dashboard's
// snapshot (the feed then keeps it fresh) and what the supervisor reads.
func (s *server) overview(w http.ResponseWriter, r *http.Request, c Caller) {
	out, err := s.buildOverview(r, c.Actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type overviewDoc struct {
	Personal []threadSummary   `json:"personal"`
	Projects []overviewProject `json:"projects"`
}

func (s *server) buildOverview(r *http.Request, actor string) (*overviewDoc, error) {
	ctx := r.Context()
	personal, err := s.personalThreads(ctx, actor, 50)
	if err != nil {
		return nil, err
	}
	list, err := s.team.ListProjects(ctx, actor)
	if err != nil {
		return nil, err
	}
	doc := &overviewDoc{Personal: nonNil(personal), Projects: []overviewProject{}}
	for _, p := range list {
		ths, err := s.projectThreads(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		doc.Projects = append(doc.Projects, overviewProject{projectView{p.Project, p.Role}, nonNil(ths)})
	}
	return doc, nil
}

func (s *server) feed(w http.ResponseWriter, r *http.Request, c Caller) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "Streaming is not supported.")
		return
	}
	sub, done := s.hub.subscribe(s.allowEvent(c.Actor))
	defer done()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(": connected\n\n"))
	fl.Flush()
	ping := time.NewTicker(s.o.PingEvery)
	defer ping.Stop()
	for {
		select {
		case ev := <-sub.ch:
			if _, err := w.Write(ev.sse(s.hub.nextID())); err != nil {
				return
			}
			fl.Flush()
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
