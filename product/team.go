package product

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/hosts/client"
)

// Team mode (Options.Team set): threads belong to projects whose members share
// them. A thread stays stored under the login of the user who created it
// (Caller.Login, what every store call is scoped by); the user who acts is
// Caller.Actor, checked against the project's membership, and is recorded as
// the author of what they send.
//
//	GET    /api/me                                  → {user, projects}
//	GET    /api/projects                            → [{project, role}]
//	POST   /api/projects  {name,repoUrl,defaultBranch,description} → 201 Project
//	GET    /api/projects/{id}                       → {project, role}
//	PATCH  /api/projects/{id}                       (owner)
//	DELETE /api/projects/{id}                       (owner)
//	GET    /api/projects/{id}/members               → [{login,name,avatarUrl,role,email?}]
//	PUT    /api/projects/{id}/members/{login} {role}   (owner)
//	DELETE /api/projects/{id}/members/{login}          (owner, or the member leaving)
//	GET    /api/projects/{id}/invites               (owner)
//	POST   /api/projects/{id}/invites {email,role}  (owner)
//	DELETE /api/projects/{id}/invites?email=        (owner)
//	GET    /api/projects/{id}/audit?limit=          (member)
//	GET    /api/overview                            → every thread the user can see, with its status
//	GET    /api/events                              → text/event-stream of FeedEvent
//	POST   /api/threads {"title","projectId"}       a project thread (member)
//	GET    /api/threads?project=ID                  the project's threads; without it, the user's personal ones

// ---- access ----

type accessError struct {
	status int
	code   string
	msg    string
}

func (e *accessError) Error() string { return e.msg }

var (
	errNotFound  = &accessError{http.StatusNotFound, "not_found", "Not found."}
	errForbidden = &accessError{http.StatusForbidden, "forbidden", "You do not have permission to do this."}
)

func isWrite(method string) bool {
	return method != http.MethodGet && method != http.MethodHead
}

// resolve turns the authenticated actor and the thread or run the request
// names into the Caller the handler works as.
func (s *server) Resolve(r *http.Request, kind resourceKind, actor string) (Caller, error) {
	c := Caller{Login: actor, Actor: actor}
	if s.team == nil || kind == kindNone {
		return c, nil
	}
	threadID := r.PathValue("id")
	if kind == kindRun {
		run, err := s.H.Store.GetRun(r.Context(), threadID)
		if err != nil {
			return c, errNotFound
		}
		threadID = run.ThreadID
	}
	need := store.RoleViewer
	if isWrite(r.Method) {
		need = store.RoleMember
	}
	return s.access(r.Context(), actor, threadID, need)
}

// access checks that actor holds at least role need on the thread and returns
// the Caller to act as: for a project thread, the thread's owner (whose
// login the store scopes it by) with the actor kept for authorship.
func (s *server) access(ctx context.Context, actor, threadID, need string) (Caller, error) {
	c := Caller{Login: actor, Actor: actor}
	meta, err := s.team.GetThreadMeta(ctx, threadID)
	if errors.Is(err, store.ErrNotFound) {
		return c, nil // not linked: the store scopes it to its owner
	}
	if err != nil {
		return c, err
	}
	c.CreatedBy, c.ProjectID = meta.CreatedBy, meta.ProjectID
	if meta.ProjectID == "" {
		if meta.Owner != actor {
			return c, errNotFound
		}
		return c, nil
	}
	role, err := s.team.MemberRole(ctx, meta.ProjectID, actor)
	if errors.Is(err, store.ErrNotFound) {
		return c, errNotFound
	}
	if err != nil {
		return c, err
	}
	if !store.RoleAtLeast(role, need) {
		return c, errForbidden
	}
	c.Login, c.Role = meta.Owner, role
	return c, nil
}

// requireProject checks the actor's role in a project.
func (s *server) requireProject(ctx context.Context, projectID, actor, need string) (string, error) {
	role, err := s.team.MemberRole(ctx, projectID, actor)
	if errors.Is(err, store.ErrNotFound) {
		return "", errNotFound
	}
	if err != nil {
		return "", err
	}
	if !store.RoleAtLeast(role, need) {
		return role, errForbidden
	}
	return role, nil
}

func (s *server) FailAccess(w http.ResponseWriter, err error) {
	var ae *accessError
	if errors.As(err, &ae) {
		writeError(w, ae.status, ae.code, ae.msg)
		return
	}
	fail(w, err)
}

// allowEvent decides whether login may see a feed event.
func (s *server) allowEvent(login string) func(FeedEvent) bool {
	return func(ev FeedEvent) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if ev.affects != "" {
			return ev.affects == login
		}
		if ev.ProjectID != "" {
			_, err := s.team.MemberRole(ctx, ev.ProjectID, login)
			return err == nil
		}
		return ev.owner == login
	}
}

// ---- views ----

// threadView is a thread as the team UI shows it.
type threadView struct {
	client.Thread
	ProjectID string `json:"projectId,omitempty"`
	CreatedBy string `json:"createdBy,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Role      string `json:"role,omitempty"`
	Status    string `json:"status"`
}

// threadSummary is a thread in a list (no messages).
type threadSummary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	ProjectID string    `json:"projectId,omitempty"`
	CreatedBy string    `json:"createdBy,omitempty"`
	Kind      string    `json:"kind,omitempty"`
	Status    string    `json:"status"`
	RunID     string    `json:"runId,omitempty"`
	Tool      string    `json:"pendingTool,omitempty"`
	Question  string    `json:"pendingQuestion,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func statusOf(t *store.Thread) string {
	switch {
	case t.PendingApproval != nil:
		return "waiting_approval"
	case t.PendingQuestion != nil:
		return "waiting_input"
	case t.ActiveRun != nil:
		return t.ActiveRun.Status
	case t.LastRun != nil && (t.LastRun.Status == store.StatusFailed || t.LastRun.Status == store.StatusInterrupted):
		return "failed"
	}
	return "idle"
}

func summarize(t *store.Thread, m store.ThreadMeta) threadSummary {
	sm := threadSummary{ID: t.ID, Title: t.Title, ProjectID: m.ProjectID, CreatedBy: m.CreatedBy, Kind: m.Kind,
		Status: statusOf(t), CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
	if t.ActiveRun != nil {
		sm.RunID = t.ActiveRun.ID
	}
	if t.PendingApproval != nil {
		sm.Tool = t.PendingApproval.ToolName
	}
	if t.PendingQuestion != nil {
		sm.Question = t.PendingQuestion.Question
	}
	return sm
}

// projectThreads lists the threads of a project the actor may see.
func (s *server) projectThreads(ctx context.Context, projectID string) ([]threadSummary, error) {
	metas, err := s.team.ListProjectThreads(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]threadSummary, 0, len(metas))
	for _, m := range metas {
		t, err := s.H.Store.GetThread(ctx, m.Owner, m.ThreadID)
		if err != nil {
			continue // deleted behind our back
		}
		out = append(out, summarize(t, m))
	}
	return out, nil
}

// personalThreads lists the actor's threads outside any project.
func (s *server) personalThreads(ctx context.Context, actor string, limit int) ([]threadSummary, error) {
	ths, err := s.H.Store.ListThreads(ctx, actor, limit*3, nil)
	if err != nil {
		return nil, err
	}
	out := make([]threadSummary, 0, len(ths))
	for _, t := range ths {
		m := store.ThreadMeta{ThreadID: t.ID, Owner: actor, CreatedBy: actor, Kind: store.ThreadChat}
		if meta, err := s.team.GetThreadMeta(ctx, t.ID); err == nil {
			if meta.ProjectID != "" || meta.Kind == store.ThreadSupervisor {
				continue
			}
			m = *meta
		}
		out = append(out, summarize(t, m))
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// withAuthors fills in the name and avatar of the authors of the messages.
func (s *server) withAuthors(ctx context.Context, th *client.Thread) {
	var logins []string
	seen := map[string]bool{}
	for _, m := range th.Messages {
		if m.Author == nil {
			continue
		}
		for _, l := range []string{m.Author.Login, m.Author.OnBehalfOf} {
			if l != "" && !seen[l] {
				seen[l] = true
				logins = append(logins, l)
			}
		}
	}
	users, err := s.team.GetUsers(ctx, logins)
	if err != nil {
		return
	}
	for i := range th.Messages {
		a := th.Messages[i].Author
		if a == nil {
			continue
		}
		if u := users[a.Login]; u != nil {
			a.Name, a.AvatarURL = u.Label(), u.Avatar()
		}
	}
}

// ---- recording what happens ----

// note records an audit entry and publishes a feed event.
func (s *server) note(c Caller, threadID, action string, detail map[string]any, ev FeedEvent) {
	if s.team == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.team.Audit(ctx, store.AuditEntry{ProjectID: c.ProjectID, ThreadID: threadID, Actor: c.Actor, Action: action, Detail: detail}); err != nil {
		slog.Warn("httpserver: audit failed", "error", err)
	}
	ev.ProjectID, ev.ThreadID, ev.Actor, ev.owner = c.ProjectID, threadID, c.Actor, c.Login
	s.hub.publish(ev)
}

// follow watches a run until it ends and publishes how.
func (s *server) follow(c Caller, run *client.Run) {
	if s.team == nil || run == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
		defer cancel()
		status := "completed"
		for ev := range s.H.Events(ctx, run.ID, 0) {
			if !agui.IsTerminal(ev.Type) {
				continue
			}
			status = outcomeOf(ev)
			break
		}
		s.hub.publish(FeedEvent{Type: "run.finished", ProjectID: c.ProjectID, ThreadID: run.ThreadID, RunID: run.ID,
			Status: status, Actor: c.Actor, owner: c.Login})
	}()
}

func outcomeOf(ev store.Event) string {
	if ev.Type == agui.RunError {
		return "failed"
	}
	var d struct {
		Outcome struct {
			Type       string `json:"type"`
			Interrupts []struct {
				Reason string `json:"reason"`
			} `json:"interrupts"`
		} `json:"outcome"`
	}
	_ = json.Unmarshal(ev.Data, &d)
	switch d.Outcome.Type {
	case "interrupt":
		if len(d.Outcome.Interrupts) > 0 && d.Outcome.Interrupts[0].Reason == "ask_user" {
			return "waiting_input"
		}
		return "waiting_approval"
	case "":
		return "completed"
	}
	return d.Outcome.Type
}
