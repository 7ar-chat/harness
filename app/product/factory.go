package product

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/store"
)

// The agent factory over HTTP. Tasks and triggers belong to a project; a
// member can create and read them, an owner schedules triggers and cancels or
// unblocks work. The GitHub webhook is the one unauthenticated route: it
// verifies the delivery's HMAC itself.
//
//	GET    /api/projects/{pid}/tasks?state=&limit=          → [Task]
//	POST   /api/projects/{pid}/tasks {title,spec,role?,model?,policy?,repo?,baseBranch?,budget?,maxAttempts?,autoMerge?,dependsOn?,priority?} → 201 Task
//	GET    /api/projects/{pid}/tasks/{tid}                  → {task, events}
//	POST   /api/projects/{pid}/tasks/{tid}/cancel           → 202
//	POST   /api/projects/{pid}/tasks/{tid}/resume {feedback} → 202   (a blocked task goes back to work)
//	GET    /api/projects/{pid}/triggers                     → [Trigger]
//	POST   /api/projects/{pid}/triggers {name,kind,schedule|event,match,template,enabled} → 201 Trigger   (owner)
//	DELETE /api/projects/{pid}/triggers/{trid}              → 204   (owner)
//	GET    /api/projects/{pid}/factory/report?days=         → spend report
//	POST   /hooks/github                                    → GitHub webhook deliveries

// FactoryOptions connects the product to the factory service of the harness.
type FactoryOptions struct {
	// Escalator is the factory.Escalator the harness was built with
	// (harness.FactoryOptions.Escalator). The server binds itself to it when it starts, so a task that
	// needs a human reaches the project's live feed and Notify.
	Escalator *FactoryEscalator
	// Hooks is the FactoryHooks the harness was built with (harness.FactoryOptions.OnSpawn); bound like Escalator.
	Hooks *FactoryHooks
}

// FactoryEscalator is a factory.Escalator that can be handed to the harness before the server exists.
type FactoryEscalator struct {
	mu sync.Mutex
	s  *server
	// Notify, when set, also messages the task owner (Telegram, mail...).
	Notify func(ctx context.Context, login, text string)
}

// Escalate implements factory.Escalator. Before the server binds it only logs.
func (e *FactoryEscalator) Escalate(ctx context.Context, t factory.Task, reason string) {
	e.mu.Lock()
	s := e.s
	e.mu.Unlock()
	slog.Warn("factory: task needs a human", "task_id", t.ID, "title", t.Title, "reason", reason)
	if s != nil {
		s.hub.publish(FeedEvent{Type: "task.blocked", ProjectID: t.ProjectID, ThreadID: t.ThreadID, Title: t.Title, Status: reason, owner: t.Owner})
		s.tellOnTelegram(ctx, t, reason)
	}
	if e.Notify != nil {
		e.Notify(ctx, t.Owner, fmt.Sprintf("Task needs you: %s\n%s", t.Title, reason))
	}
}

func (e *FactoryEscalator) bind(s *server) {
	e.mu.Lock()
	e.s = s
	e.mu.Unlock()
}

// FactoryHooks links the threads the factory spawns to the project of their task, so the project's members can open
// them. It is handed to the harness (OnSpawn) before the server exists; the linking needs only the team store, so a
// task dispatched while the server is still starting is linked too. The server binds itself for the live feed.
type FactoryHooks struct {
	// Team is the store the thread's project link is written to.
	Team store.TeamStore

	mu sync.Mutex
	s  *server
}

// OnSpawn is harness.FactoryOptions.OnSpawn. A task outside a project keeps its threads private to the owner.
// A failure is logged: the agent must still run, only the project's members would not see the thread.
func (h *FactoryHooks) OnSpawn(ctx context.Context, threadID string, t factory.Task, req factory.SpawnRequest) {
	if t.ProjectID == "" || h.Team == nil {
		return
	}
	meta := store.ThreadMeta{ThreadID: threadID, ProjectID: t.ProjectID, Owner: req.Login, CreatedBy: t.Owner, Kind: store.ThreadChat}
	if err := h.Team.SetThreadMeta(ctx, meta); err != nil {
		slog.Warn("factory: linking a task thread to its project failed", "task_id", t.ID, "thread_id", threadID, "error", err)
		return
	}
	h.mu.Lock()
	s := h.s
	h.mu.Unlock()
	if s != nil {
		s.hub.publish(FeedEvent{Type: "thread.created", ProjectID: t.ProjectID, ThreadID: threadID, Actor: t.Owner, Title: req.Title, owner: req.Login})
	}
}

func (h *FactoryHooks) bind(s *server) {
	h.mu.Lock()
	h.s = s
	h.mu.Unlock()
}

// FactoryAuth adapts a GitHub App to factory.RepoAuth: one installation token
// limited to the repository, found through the App's installation on it.
type FactoryAuth struct {
	App *GitHubApp
}

// Token implements factory.RepoAuth.
func (a FactoryAuth) Token(ctx context.Context, repo string) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("factory auth: %q is not owner/name", repo)
	}
	inst, err := a.App.RepoInstallation(ctx, owner, name)
	if err != nil {
		return "", fmt.Errorf("factory auth: installation for %s: %w", repo, err)
	}
	return a.App.InstallationToken(ctx, inst, name)
}

// tellOnTelegram messages the task owner's linked Telegram chat; without the bridge or a linked chat it does nothing.
func (s *server) tellOnTelegram(ctx context.Context, t factory.Task, reason string) {
	if s.tg == nil || s.team == nil {
		return
	}
	cn, err := s.team.GetConnector(ctx, t.Owner, store.ConnectorTelegram)
	if err != nil {
		return
	}
	text := fmt.Sprintf("Task needs you: %s\n%s", t.Title, reason)
	if base := s.configuredBase(); base != "" && t.ProjectID != "" {
		text += "\n" + base + "/#/projects/" + t.ProjectID
	}
	if err := s.tg.send(ctx, cn.Account, text, nil); err != nil {
		slog.Warn("factory: telegram escalation failed", "task_id", t.ID, "error", err)
	}
}

// factoryService is the harness's factory, nil when the feature is off.
func (s *server) factory() *harness.Factory { return s.H.FactoryOf() }

func (s *server) routeFactory() {
	if s.factory() == nil {
		return
	}
	if e := s.opt.Factory.Escalator; e != nil {
		e.bind(s)
	}
	if h := s.opt.Factory.Hooks; h != nil {
		h.bind(s)
	}
	s.Route("GET /projects/{pid}/tasks", s.listTasks)
	s.Route("POST /projects/{pid}/tasks", s.createTask)
	s.Route("GET /projects/{pid}/tasks/{tid}", s.getTask)
	s.Route("POST /projects/{pid}/tasks/{tid}/cancel", s.cancelTask)
	s.Route("POST /projects/{pid}/tasks/{tid}/resume", s.resumeTask)
	s.Route("GET /projects/{pid}/triggers", s.listTriggers)
	s.Route("POST /projects/{pid}/triggers", s.createTrigger)
	s.Route("DELETE /projects/{pid}/triggers/{trid}", s.deleteTrigger)
	s.Route("GET /projects/{pid}/factory/report", s.factoryReport)
	s.Mux.Handle("POST /hooks/github", s.factory().WebhookHandler())
	s.Mux.Handle("POST /api/hooks/github", s.factory().WebhookHandler())
}

// inProject is the caller of a project route for the audit log and the feed: those routes resolve no thread, so
// the Caller carries no project and its events would reach nobody but the actor.
func inProject(c Caller, projectID string) Caller {
	c.ProjectID = projectID
	return c
}

// taskOf loads the task of the path and checks it belongs to the project.
func (s *server) taskOf(w http.ResponseWriter, r *http.Request, pid string) (*factory.Task, bool) {
	t, err := s.factory().Tasks.Get(r.Context(), r.PathValue("tid"))
	if errors.Is(err, factory.ErrNotFound) || err == nil && t.ProjectID != pid {
		s.FailAccess(w, errNotFound)
		return nil, false
	}
	if err != nil {
		fail(w, err)
		return nil, false
	}
	return t, true
}

func (s *server) listTasks(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	f := factory.Filter{ProjectID: p.ID, Limit: min(limitParam(r, 100), 500)}
	if st := r.URL.Query().Get("state"); st != "" {
		for _, v := range strings.Split(st, ",") {
			if state := factory.State(v); state.Valid() {
				f.States = append(f.States, state)
			}
		}
	}
	ts, err := s.factory().Tasks.List(r.Context(), f)
	if err != nil {
		fail(w, err)
		return
	}
	if ts == nil {
		ts = []factory.Task{}
	}
	writeJSON(w, http.StatusOK, ts)
}

type taskBody struct {
	Title       string         `json:"title"`
	Spec        string         `json:"spec"`
	Role        string         `json:"role"`
	Model       string         `json:"model"`
	Policy      string         `json:"policy"`
	Repo        string         `json:"repo"`
	BaseBranch  string         `json:"baseBranch"`
	Budget      factory.Budget `json:"budget"`
	MaxAttempts int            `json:"maxAttempts"`
	AutoMerge   bool           `json:"autoMerge"`
	DependsOn   []string       `json:"dependsOn"`
	Labels      []string       `json:"labels"`
	Priority    int            `json:"priority"`
}

func (s *server) createTask(w http.ResponseWriter, r *http.Request, c Caller) {
	p, role, ok := s.project(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	var b taskBody
	if !decode(w, r, &b) {
		return
	}
	switch b.Policy {
	case "", factory.PolicySupervised, factory.PolicySandboxed, factory.PolicyAutonomous:
	default:
		writeError(w, http.StatusBadRequest, "invalid_policy", "policy must be supervised, sandboxed or autonomous.")
		return
	}
	// The run's GitHub token is minted for the task's repository, so it is always the project's own.
	repo, ok := scopeRepo(w, p, b.Repo)
	if !ok {
		return
	}
	// Merging without a human is the owner's decision.
	if b.AutoMerge && !store.RoleAtLeast(role, store.RoleOwner) {
		writeError(w, http.StatusForbidden, "forbidden", "only a project owner can allow automatic merging.")
		return
	}
	base := b.BaseBranch
	if base == "" {
		base = p.DefaultBranch
	}
	t, _, err := s.factory().Tasks.Create(r.Context(), factory.Task{
		ProjectID: p.ID, Owner: c.Actor, Title: b.Title, Spec: b.Spec, Role: b.Role, Model: b.Model, Policy: b.Policy,
		Repo: repo, BaseBranch: base, Budget: b.Budget, MaxAttempts: b.MaxAttempts, AutoMerge: b.AutoMerge,
		DependsOn: b.DependsOn, Labels: b.Labels, Priority: b.Priority,
		Source: factory.Source{Kind: factory.SourceManual},
	})
	if errors.Is(err, factory.ErrInvalid) {
		writeError(w, http.StatusBadRequest, "invalid_task", err.Error())
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.factory().Tasks.Kick()
	s.note(inProject(c, p.ID), "", "task.create", map[string]any{"task": t.ID, "title": t.Title}, FeedEvent{Type: "task.created", Title: t.Title})
	writeJSON(w, http.StatusCreated, t)
}

func (s *server) getTask(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	t, ok := s.taskOf(w, r, p.ID)
	if !ok {
		return
	}
	ev, err := s.factory().Tasks.Events(r.Context(), t.ID, 200)
	if err != nil {
		fail(w, err)
		return
	}
	if ev == nil {
		ev = []factory.TaskEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": t, "events": ev})
}

func (s *server) cancelTask(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	t, ok := s.taskOf(w, r, p.ID)
	if !ok {
		return
	}
	if err := s.factory().Tasks.Cancel(r.Context(), t.ID); err != nil {
		if errors.Is(err, factory.ErrConflict) {
			writeError(w, http.StatusConflict, "conflict", "The task already finished.")
			return
		}
		fail(w, err)
		return
	}
	s.note(inProject(c, p.ID), t.ThreadID, "task.cancel", map[string]any{"task": t.ID}, FeedEvent{Type: "task.updated", Title: t.Title})
	w.WriteHeader(http.StatusAccepted)
}

func (s *server) resumeTask(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	t, ok := s.taskOf(w, r, p.ID)
	if !ok {
		return
	}
	var b struct {
		Feedback string `json:"feedback"`
	}
	if !decode(w, r, &b) {
		return
	}
	if t.State != factory.StateBlocked {
		writeError(w, http.StatusConflict, "conflict", "Only a blocked task can be resumed.")
		return
	}
	feedback := strings.TrimSpace(b.Feedback)
	if feedback == "" {
		feedback = "A human unblocked this task. Continue."
	} else {
		feedback = "A human says: " + feedback
	}
	if err := s.factory().Tasks.Resume(r.Context(), t.ID, feedback); err != nil {
		fail(w, err)
		return
	}
	s.factory().Tasks.Kick()
	s.note(inProject(c, p.ID), t.ThreadID, "task.resume", map[string]any{"task": t.ID}, FeedEvent{Type: "task.updated", Title: t.Title})
	w.WriteHeader(http.StatusAccepted)
}

func (s *server) listTriggers(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	ts, err := s.factory().Store.ListTriggers(r.Context(), p.ID)
	if err != nil {
		fail(w, err)
		return
	}
	if ts == nil {
		ts = []factory.Trigger{}
	}
	writeJSON(w, http.StatusOK, ts)
}

// projectRepo is the project's own repository as "owner/name", spelled as the URL spells it ("" when the project
// has none). Every task and trigger of the project is bound to it: the GitHub App token minted for a run reaches
// whatever repository the App is installed on, so the repository must never come from the request.
func projectRepo(p *store.Project) string {
	if o, n, ok := ParseRepo(p.RepoURL); ok {
		return o + "/" + n
	}
	return ""
}

// scopeRepo checks a repository named in a request against the project's own: empty or the same repository
// (case-insensitively) is accepted and yields the project's spelling; anything else is refused with 400 invalid_repo.
func scopeRepo(w http.ResponseWriter, p *store.Project, named string) (string, bool) {
	own := projectRepo(p)
	named = strings.TrimSpace(named)
	if named == "" || own != "" && strings.EqualFold(named, own) {
		return own, true
	}
	writeError(w, http.StatusBadRequest, "invalid_repo", "tasks run in the project's own repository.")
	return "", false
}

func (s *server) createTrigger(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleOwner)
	if !ok {
		return
	}
	// A trigger without "enabled" is on: the documented body has no such field and a stored-off trigger never fires.
	var body struct {
		factory.Trigger
		Enabled *bool `json:"enabled"`
	}
	if !decode(w, r, &body) {
		return
	}
	t := body.Trigger
	t.ID, t.ProjectID, t.Owner, t.Enabled = "", p.ID, c.Actor, body.Enabled == nil || *body.Enabled
	if strings.TrimSpace(t.Name) == "" || strings.TrimSpace(t.Template.Title) == "" {
		writeError(w, http.StatusBadRequest, "invalid_trigger", "name and template.title are required.")
		return
	}
	// The trigger's tasks run in the project's repository and nowhere else (a template is rejected, not rendered).
	repo, ok := scopeRepo(w, p, t.Template.Repo)
	if !ok {
		return
	}
	t.Template.Repo = repo
	if t.Template.BaseBranch == "" {
		t.Template.BaseBranch = p.DefaultBranch
	}
	switch t.Kind {
	case factory.TriggerCron:
	case factory.TriggerWebhook:
		if strings.TrimSpace(t.Event) == "" {
			writeError(w, http.StatusBadRequest, "invalid_trigger", "a webhook trigger needs an event, for example issues.labeled.")
			return
		}
		if repo == "" {
			writeError(w, http.StatusBadRequest, "invalid_trigger", "connect a repository first: a webhook trigger fires only for events of the project's repository.")
			return
		}
		// A webhook delivery carries events of every repository the App is installed on; only this project's reach it.
		match := make(map[string]string, len(t.Match)+1)
		for k, v := range t.Match {
			match[k] = v
		}
		match["repo"] = repo
		t.Match = match
	default:
		writeError(w, http.StatusBadRequest, "invalid_trigger", "kind must be cron or webhook.")
		return
	}
	// Register validates the schedule and renders the template once, for both kinds.
	out, err := s.factory().Triggers.Register(r.Context(), t)
	if errors.Is(err, factory.ErrInvalid) {
		writeError(w, http.StatusBadRequest, "invalid_trigger", err.Error())
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.note(inProject(c, p.ID), "", "trigger.create", map[string]any{"trigger": out.ID, "name": out.Name}, FeedEvent{Type: "project.changed"})
	writeJSON(w, http.StatusCreated, out)
}

func (s *server) deleteTrigger(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleOwner)
	if !ok {
		return
	}
	tr, err := s.factory().Store.GetTrigger(r.Context(), r.PathValue("trid"))
	if errors.Is(err, factory.ErrNotFound) || err == nil && tr.ProjectID != p.ID {
		s.FailAccess(w, errNotFound)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	if err := s.factory().Store.DeleteTrigger(r.Context(), tr.ID); err != nil {
		fail(w, err)
		return
	}
	s.note(inProject(c, p.ID), "", "trigger.delete", map[string]any{"trigger": tr.ID}, FeedEvent{Type: "project.changed"})
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) factoryReport(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	days := 7
	if n, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && n > 0 {
		days = min(n, 90)
	}
	rep, err := s.factory().Report(r.Context(), p.ID, time.Now().Add(-time.Duration(days)*24*time.Hour))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
