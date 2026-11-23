package product

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/requirements"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/hosts/client"
)

// Requirements tracking: what a project wants done, followed from the idea to the merged pull request
// (requirement, then the sessions and factory tasks that work on it, then the PR, then done). Served when
// Options.Requirements is set.
//
//	GET    /api/projects/{pid}/requirements?status=&label=&q=          → [Requirement]
//	POST   /api/projects/{pid}/requirements {title,body?,priority?,labels?,status?} → 201 Requirement        (member)
//	GET    /api/projects/{pid}/requirements/{rid}                      → {requirement, tasks, threads}
//	PATCH  /api/projects/{pid}/requirements/{rid} {title?,body?,priority?,labels?,status?} → Requirement      (member)
//	DELETE /api/projects/{pid}/requirements/{rid}                      → 204                                  (owner)
//	POST   /api/projects/{pid}/requirements/{rid}/dispatch {policy?,budget?} → 202 Requirement                (member)
//	POST   /api/projects/{pid}/requirements/import/github {label?}     → {imported, skipped, requirements}    (member)
//	POST   /api/projects/{pid}/requirements/import/issues {label?}     → {imported, skipped, requirements}    (member; from the repository's own host: GitHub, GitLab, Gitea; see githosts.go)
//	GET    /api/projects/{pid}/requirements/settings                   → RequirementSettings
//	PUT    /api/projects/{pid}/requirements/settings {autoDispatch,policy?,budget?} → RequirementSettings      (owner)
//	POST   /api/threads/{id}/requirement {requirementId}               → Requirement   (links the conversation)
//
// Dispatch creates a factory task from the requirement when the factory is on, else starts a project session with
// the requirement as its prompt, and links it. The factory's task hook then moves the requirement: task started
// is in_progress, pull request opened in_review, task done (the PR merged) done; a failed or blocked task keeps it
// in progress with a flag. With "auto-dispatch" on, a requirement moved to ready is dispatched at once with the
// project's policy and budget.

// RequirementDetail is a requirement with the sessions and tasks linked to it.
type RequirementDetail struct {
	Requirement requirements.Requirement `json:"requirement"`
	Tasks       []RequirementTask        `json:"tasks"`
	Threads     []RequirementThread      `json:"threads"`
}

// RequirementTask is a linked factory task.
type RequirementTask struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	State    string `json:"state"`
	PRNumber int    `json:"prNumber,omitempty"`
	PRURL    string `json:"prUrl,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// RequirementThread is a linked session.
type RequirementThread struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// RequirementImport is the answer of the GitHub import.
type RequirementImport struct {
	Imported     int                        `json:"imported"`
	Skipped      int                        `json:"skipped"`
	Requirements []requirements.Requirement `json:"requirements"`
}

func (s *server) routeRequirements() {
	if s.opt.Requirements == nil {
		return
	}
	s.Route("GET /projects/{pid}/requirements", s.listRequirements)
	s.Route("POST /projects/{pid}/requirements", s.createRequirement)
	s.Route("GET /projects/{pid}/requirements/settings", s.getRequirementSettings)
	s.Route("PUT /projects/{pid}/requirements/settings", s.putRequirementSettings)
	s.Route("POST /projects/{pid}/requirements/import/github", s.importRequirements)
	s.Route("POST /projects/{pid}/requirements/import/issues", s.importIssues)
	s.Route("GET /projects/{pid}/requirements/{rid}", s.getRequirement)
	s.Route("PATCH /projects/{pid}/requirements/{rid}", s.patchRequirement)
	s.Route("DELETE /projects/{pid}/requirements/{rid}", s.deleteRequirement)
	s.Route("POST /projects/{pid}/requirements/{rid}/dispatch", s.dispatchRequirementRoute)
	s.Route("POST /threads/{id}/requirement", s.linkThreadRequirement)
}

// requirementOf loads the requirement of the path and checks it belongs to the project.
func (s *server) requirementOf(w http.ResponseWriter, r *http.Request, pid string) (*requirements.Requirement, bool) {
	req, err := s.opt.Requirements.Get(r.Context(), r.PathValue("rid"))
	if errors.Is(err, requirements.ErrNotFound) || err == nil && req.ProjectID != pid {
		s.FailAccess(w, errNotFound)
		return nil, false
	}
	if err != nil {
		fail(w, err)
		return nil, false
	}
	return req, true
}

func requirementError(w http.ResponseWriter, err error) {
	if errors.Is(err, requirements.ErrInvalid) {
		msg := err.Error()
		if i := strings.LastIndex(msg, "\n"); i >= 0 {
			msg = msg[i+1:]
		}
		writeError(w, http.StatusBadRequest, "invalid_requirement", msg)
		return
	}
	if errors.Is(err, requirements.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	fail(w, err)
}

func (s *server) listRequirements(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := requirements.Filter{ProjectID: p.ID, Label: q.Get("label"), Query: q.Get("q"), Limit: min(limitParam(r, 500), 500)}
	for _, v := range strings.Split(q.Get("status"), ",") {
		if st := requirements.Status(strings.TrimSpace(v)); st.Valid() {
			f.Statuses = append(f.Statuses, st)
		}
	}
	list, err := s.opt.Requirements.List(r.Context(), f)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nonNil(list))
}

type requirementBody struct {
	Title    *string   `json:"title"`
	Body     *string   `json:"body"`
	Priority *int      `json:"priority"`
	Labels   *[]string `json:"labels"`
	Status   *string   `json:"status"`
}

func (s *server) createRequirement(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	var b requirementBody
	if !decode(w, r, &b) {
		return
	}
	req := requirements.Requirement{ProjectID: p.ID, Author: c.Actor, Source: requirements.Origin{Kind: requirements.SourceManual}}
	if b.Title != nil {
		req.Title = *b.Title
	}
	if b.Body != nil {
		req.Body = *b.Body
	}
	if b.Priority != nil {
		req.Priority = *b.Priority
	}
	if b.Labels != nil {
		req.Labels = *b.Labels
	}
	if b.Status != nil {
		req.Status = requirements.Status(*b.Status)
	}
	created, _, err := s.opt.Requirements.Create(r.Context(), req)
	if err != nil {
		requirementError(w, err)
		return
	}
	s.note(inProject(c, p.ID), "", "requirement.create", map[string]any{"requirement": created.ID, "title": created.Title}, FeedEvent{Type: "requirement.changed", Title: created.Title})
	if created.Status == requirements.StatusReady {
		created = s.autoDispatch(r.Context(), c, p, created)
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *server) getRequirement(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	req, ok := s.requirementOf(w, r, p.ID)
	if !ok {
		return
	}
	d := RequirementDetail{Requirement: *req, Tasks: []RequirementTask{}, Threads: []RequirementThread{}}
	if f := s.factory(); f != nil {
		for _, id := range req.TaskIDs {
			t, err := f.Tasks.Get(r.Context(), id)
			if err != nil || t.ProjectID != p.ID {
				continue
			}
			rt := RequirementTask{ID: t.ID, Title: t.Title, State: string(t.State), PRNumber: t.PR.Number, PRURL: t.PR.URL, Reason: firstNonEmpty(t.BlockedReason, t.Error)}
			d.Tasks = append(d.Tasks, rt)
		}
	}
	for _, id := range req.ThreadIDs {
		meta, err := s.team.GetThreadMeta(r.Context(), id)
		if err != nil || meta.ProjectID != p.ID {
			continue
		}
		th, err := s.H.Store.GetThread(r.Context(), meta.Owner, id)
		if err != nil {
			continue
		}
		sum := summarize(th, *meta)
		d.Threads = append(d.Threads, RequirementThread{ID: id, Title: sum.Title, Status: sum.Status})
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *server) patchRequirement(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	cur, ok := s.requirementOf(w, r, p.ID)
	if !ok {
		return
	}
	var b requirementBody
	if !decode(w, r, &b) {
		return
	}
	patch := requirements.Patch{Title: b.Title, Body: b.Body, Priority: b.Priority, Labels: b.Labels}
	if b.Status != nil {
		st := requirements.Status(*b.Status)
		patch.Status = &st
		if st != requirements.StatusInProgress && st != requirements.StatusInReview { // a human decision clears a stale flag
			empty := ""
			patch.Flag = &empty
		}
	}
	up, err := s.opt.Requirements.Update(r.Context(), cur.ID, patch)
	if err != nil {
		requirementError(w, err)
		return
	}
	s.note(inProject(c, p.ID), "", "requirement.update", map[string]any{"requirement": up.ID, "status": string(up.Status)}, FeedEvent{Type: "requirement.changed", Title: up.Title})
	if up.Status == requirements.StatusReady && cur.Status != requirements.StatusReady {
		up = s.autoDispatch(r.Context(), c, p, up)
	}
	writeJSON(w, http.StatusOK, up)
}

func (s *server) deleteRequirement(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleOwner)
	if !ok {
		return
	}
	req, ok := s.requirementOf(w, r, p.ID)
	if !ok {
		return
	}
	if err := s.opt.Requirements.Delete(r.Context(), req.ID); err != nil {
		requirementError(w, err)
		return
	}
	s.note(inProject(c, p.ID), "", "requirement.delete", map[string]any{"requirement": req.ID, "title": req.Title}, FeedEvent{Type: "requirement.changed", Title: req.Title})
	w.WriteHeader(http.StatusNoContent)
}

// ---- settings ----

func (s *server) getRequirementSettings(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	st, err := s.opt.Requirements.GetSettings(r.Context(), p.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *server) putRequirementSettings(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleOwner)
	if !ok {
		return
	}
	var st requirements.Settings
	if !decode(w, r, &st) {
		return
	}
	st.ProjectID = p.ID
	switch st.Policy {
	case "", factory.PolicySupervised, factory.PolicySandboxed, factory.PolicyAutonomous:
	default:
		writeError(w, http.StatusBadRequest, "invalid_policy", "policy must be supervised, sandboxed or autonomous.")
		return
	}
	if err := validBudget(factory.Budget(st.Budget)); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_budget", err.Error())
		return
	}
	if st.AutoDispatch {
		switch {
		case s.factory() == nil:
			writeError(w, http.StatusBadRequest, "no_factory", "Auto-dispatch needs the agent factory.")
			return
		case st.Budget.MaxUSD <= 0 && st.Budget.MaxTokens <= 0:
			writeError(w, http.StatusBadRequest, "budget_required", "Auto-dispatch needs a budget: set a dollar or a token cap.")
			return
		}
	}
	if err := s.opt.Requirements.PutSettings(r.Context(), st); err != nil {
		requirementError(w, err)
		return
	}
	s.note(inProject(c, p.ID), "", "requirement.settings", map[string]any{"autoDispatch": st.AutoDispatch}, FeedEvent{Type: "requirement.changed"})
	writeJSON(w, http.StatusOK, st)
}

// ---- dispatch ----

// dispatchError is a refusal the route answers with a status.
type dispatchError struct {
	status int
	code   string
	msg    string
}

func (e *dispatchError) Error() string { return e.msg }

// dispatch starts the work on req: a factory task when the factory is on, else a session of the project with the
// requirement as its prompt. It links what it started and moves the requirement to in_progress.
func (s *server) dispatch(ctx context.Context, c Caller, p *store.Project, req *requirements.Requirement, policy string, budget factory.Budget) (*requirements.Requirement, error) {
	switch {
	case req.Status.Closed():
		return nil, &dispatchError{http.StatusConflict, "conflict", "The requirement is already " + string(req.Status) + "."}
	case (req.Status == requirements.StatusInProgress || req.Status == requirements.StatusInReview) && req.Flag == "":
		return nil, &dispatchError{http.StatusConflict, "conflict", "The requirement is already being worked on."}
	}
	prompt := requirementPrompt(req)
	var patch requirements.Patch
	if f := s.factory(); f != nil {
		t, _, err := f.Tasks.Create(ctx, factory.Task{
			ProjectID: p.ID, Owner: c.Actor, Title: req.Title, Spec: prompt, Policy: policy,
			Repo: projectRepo(p), BaseBranch: p.DefaultBranch, Budget: budget, Labels: req.Labels, Priority: req.Priority,
			Source: factory.Source{Kind: factory.SourceManual},
		})
		if errors.Is(err, factory.ErrInvalid) {
			return nil, &dispatchError{http.StatusBadRequest, "invalid_task", err.Error()}
		}
		if err != nil {
			return nil, err
		}
		f.Tasks.Kick()
		patch.AddTaskID = t.ID
		s.note(inProject(c, p.ID), "", "task.create", map[string]any{"task": t.ID, "title": t.Title, "requirement": req.ID}, FeedEvent{Type: "task.created", Title: t.Title})
	} else {
		th, err := s.H.Store.CreateThread(ctx, c.Actor, req.Title)
		if err != nil {
			return nil, err
		}
		meta := store.ThreadMeta{ThreadID: th.ID, ProjectID: p.ID, Owner: c.Actor, CreatedBy: c.Actor, Kind: store.ThreadChat}
		if err := s.team.SetThreadMeta(ctx, meta); err != nil {
			_, _ = s.H.Store.DeleteThread(ctx, c.Actor, th.ID)
			return nil, err
		}
		pc := Caller{Login: c.Actor, Actor: c.Actor, ProjectID: p.ID}
		run, err := s.API(pc).Send(ctx, th.ID, prompt, client.SendOptions{})
		if err != nil {
			return nil, err
		}
		s.note(pc, th.ID, "thread.create", map[string]any{"requirement": req.ID}, FeedEvent{Type: "thread.created", Title: th.Title})
		s.AfterRun(pc, run, "requirement.dispatch", map[string]any{"requirement": req.ID})
		patch.AddThreadID = th.ID
	}
	st, empty := requirements.StatusInProgress, ""
	patch.Status, patch.Flag = &st, &empty
	return s.opt.Requirements.Update(ctx, req.ID, patch)
}

// requirementPrompt is what the agent reads: the requirement and where to report it.
func requirementPrompt(req *requirements.Requirement) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Requirement %s: %s\n", req.ID, req.Title)
	if req.Source.URL != "" {
		fmt.Fprintf(&b, "Source: %s\n", req.Source.URL)
	}
	if body := strings.TrimSpace(req.Body); body != "" {
		b.WriteString("\n" + body + "\n")
	}
	fmt.Fprintf(&b, "\nImplement this requirement. When you open the pull request, mention requirement %s in its description.", req.ID)
	return b.String()
}

func (s *server) dispatchRequirementRoute(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	req, ok := s.requirementOf(w, r, p.ID)
	if !ok {
		return
	}
	var b struct {
		Policy string          `json:"policy"`
		Budget *factory.Budget `json:"budget"`
	}
	if !decode(w, r, &b) {
		return
	}
	switch b.Policy {
	case "", factory.PolicySupervised, factory.PolicySandboxed, factory.PolicyAutonomous:
	default:
		writeError(w, http.StatusBadRequest, "invalid_policy", "policy must be supervised, sandboxed or autonomous.")
		return
	}
	st, err := s.opt.Requirements.GetSettings(r.Context(), p.ID)
	if err != nil {
		fail(w, err)
		return
	}
	policy, budget := firstNonEmpty(b.Policy, st.Policy), factory.Budget(st.Budget)
	if b.Budget != nil {
		budget = *b.Budget
	}
	if err := validBudget(budget); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_budget", err.Error())
		return
	}
	up, err := s.dispatch(r.Context(), c, p, req, policy, budget)
	var de *dispatchError
	if errors.As(err, &de) {
		writeError(w, de.status, de.code, de.msg)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.note(inProject(c, p.ID), "", "requirement.dispatch", map[string]any{"requirement": up.ID}, FeedEvent{Type: "requirement.changed", Title: up.Title})
	writeJSON(w, http.StatusAccepted, up)
}

// autoDispatch dispatches a requirement that just became ready when the project asked for it. It needs the factory
// and a budget (the settings refuse to turn it on without them, this checks again); a refusal or a failure is
// written on the requirement as a flag, never lost.
func (s *server) autoDispatch(ctx context.Context, c Caller, p *store.Project, req *requirements.Requirement) *requirements.Requirement {
	st, err := s.opt.Requirements.GetSettings(ctx, p.ID)
	if err != nil || !st.AutoDispatch || s.factory() == nil {
		return req
	}
	flag := func(msg string) *requirements.Requirement {
		up, err := s.opt.Requirements.Update(ctx, req.ID, requirements.Patch{Flag: &msg})
		if err != nil {
			return req
		}
		return up
	}
	if st.Budget.MaxUSD <= 0 && st.Budget.MaxTokens <= 0 {
		return flag("auto-dispatch skipped: the project has no budget set")
	}
	up, err := s.dispatch(ctx, c, p, req, st.Policy, factory.Budget(st.Budget))
	if err != nil {
		slog.Warn("requirements: auto-dispatch failed", "requirement", req.ID, "error", err)
		return flag("auto-dispatch failed: " + err.Error())
	}
	s.note(inProject(c, p.ID), "", "requirement.dispatch", map[string]any{"requirement": up.ID, "auto": true}, FeedEvent{Type: "requirement.changed", Title: up.Title})
	return up
}

// ---- conversation link ----

func (s *server) linkThreadRequirement(w http.ResponseWriter, r *http.Request, c Caller) {
	if c.ProjectID == "" {
		writeError(w, http.StatusBadRequest, "no_project", "Only a session of a project can be linked to a requirement.")
		return
	}
	var b struct {
		RequirementID string `json:"requirementId"`
	}
	if !decode(w, r, &b) {
		return
	}
	req, err := s.opt.Requirements.Get(r.Context(), b.RequirementID)
	if errors.Is(err, requirements.ErrNotFound) || err == nil && req.ProjectID != c.ProjectID {
		writeError(w, http.StatusNotFound, "not_found", "No such requirement in this project.")
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	patch := requirements.Patch{AddThreadID: r.PathValue("id")}
	if req.Status == requirements.StatusBacklog || req.Status == requirements.StatusReady {
		st := requirements.StatusInProgress
		patch.Status = &st
	}
	up, err := s.opt.Requirements.Update(r.Context(), req.ID, patch)
	if err != nil {
		requirementError(w, err)
		return
	}
	s.note(c, r.PathValue("id"), "requirement.link", map[string]any{"requirement": req.ID}, FeedEvent{Type: "requirement.changed", Title: up.Title})
	writeJSON(w, http.StatusOK, up)
}

// RequirementsFeature shows the requirement a conversation is linked to in the context of each turn, so the agent
// knows what it is working on and can name it in its pull request. Only the id, the title, the status and the pull
// request are shown: the text of a requirement (which may come from an issue) is not put in the prompt.
func RequirementsFeature(rs requirements.Store) harness.Feature {
	return harness.FeatureFunc("requirements", func(b *harness.Builder) error {
		b.ContextSection(func(ctx context.Context, in agent.Input) string {
			if rs == nil || in.Run == nil {
				return ""
			}
			list, err := rs.List(ctx, requirements.Filter{ThreadID: in.Run.ThreadID, Limit: 3})
			if err != nil || len(list) == 0 {
				return ""
			}
			var sb strings.Builder
			sb.WriteString("# Linked requirement\n")
			for _, r := range list {
				fmt.Fprintf(&sb, "This conversation works on requirement %s: %q (status: %s", r.ID, r.Title, r.Status)
				if r.PR.URL != "" {
					fmt.Fprintf(&sb, ", pull request %s", r.PR.URL)
				}
				sb.WriteString("). Mention the requirement id in the description of the pull request you open.\n")
			}
			return sb.String()
		})
		return nil
	})
}

// requirementsByThread maps the sessions of a project to the requirement they work on, by the thread links and by
// the tasks (a task's thread is the session that carries it). Nil without requirements tracking.
func (s *server) requirementsByThread(ctx context.Context, projectID string, byThread map[string]*factory.Task) map[string]requirements.Requirement {
	if s.opt.Requirements == nil {
		return nil
	}
	list, err := s.opt.Requirements.List(ctx, requirements.Filter{ProjectID: projectID})
	if err != nil {
		return nil
	}
	out := map[string]requirements.Requirement{}
	for _, r := range list {
		for _, id := range r.ThreadIDs {
			if _, taken := out[id]; !taken {
				out[id] = r
			}
		}
	}
	for thread, task := range byThread {
		if _, taken := out[thread]; taken {
			continue
		}
		for _, r := range list {
			for _, id := range r.TaskIDs {
				if id == task.ID {
					out[thread] = r
				}
			}
		}
	}
	return out
}

// ---- completion by merge ----

// requirementTaskMoved moves the requirements linked to a task (by task, or by the session the task is) when the
// task changes state: started is in progress, a pull request opened is in review, done (the merge) is done, and a
// failed, blocked or cancelled task keeps the requirement where it is, flagged. It is called by the factory's
// OnTaskState hook, so nothing polls.
func (s *server) requirementTaskMoved(t factory.Task, to factory.State) {
	rs := s.opt.Requirements
	if rs == nil || t.ProjectID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	seen := map[string]bool{}
	var linked []requirements.Requirement
	for _, f := range []requirements.Filter{{ProjectID: t.ProjectID, TaskID: t.ID}, {ProjectID: t.ProjectID, ThreadID: t.ThreadID}} {
		if f.ThreadID == "" && f.TaskID == "" {
			continue
		}
		list, err := rs.List(ctx, f)
		if err != nil {
			slog.Warn("requirements: finding the requirement of a task failed", "task_id", t.ID, "error", err)
			return
		}
		for _, r := range list {
			if !seen[r.ID] {
				seen[r.ID] = true
				linked = append(linked, r)
			}
		}
	}
	for _, r := range linked {
		patch, ok := taskMove(r, t, to)
		if !ok {
			continue
		}
		patch.AddTaskID = t.ID
		if t.ThreadID != "" {
			patch.AddThreadID = t.ThreadID
		}
		if _, err := rs.Update(ctx, r.ID, patch); err != nil {
			slog.Warn("requirements: moving a requirement failed", "requirement", r.ID, "task_id", t.ID, "error", err)
			continue
		}
		s.hub.publish(FeedEvent{Type: "requirement.changed", ProjectID: t.ProjectID, Title: r.Title, owner: t.Owner})
	}
}

// taskMove is the requirement patch a task state change asks for; false when the requirement stays as it is.
func taskMove(r requirements.Requirement, t factory.Task, to factory.State) (requirements.Patch, bool) {
	var p requirements.Patch
	if r.Status == requirements.StatusRejected || r.Status == requirements.StatusDone && to != factory.StateDone {
		return p, false // closed by a human, or already delivered
	}
	set := func(st requirements.Status) { p.Status = &st }
	flag := func(f string) { p.Flag = &f }
	started := r.Status == requirements.StatusBacklog || r.Status == requirements.StatusReady
	switch to {
	case factory.StatePending, factory.StateReady, factory.StateRunning:
		set(requirements.StatusInProgress)
		flag("")
	case factory.StateReview:
		set(requirements.StatusInReview)
		flag("")
		p.PR = &requirements.LinkedPR{Number: t.PR.Number, URL: t.PR.URL, State: "open"}
	case factory.StateBlocked:
		if started {
			set(requirements.StatusInProgress)
		}
		flag("task blocked: " + t.BlockedReason)
		if t.PR.Number > 0 {
			p.PR = &requirements.LinkedPR{Number: t.PR.Number, URL: t.PR.URL, State: "open"}
		}
	case factory.StateFailed:
		if started {
			set(requirements.StatusInProgress)
		}
		flag("task failed: " + t.Error)
	case factory.StateCancelled:
		if started {
			set(requirements.StatusInProgress)
		}
		flag("task cancelled")
	case factory.StateDone:
		set(requirements.StatusDone)
		flag("")
		if t.PR.Number > 0 {
			p.PR = &requirements.LinkedPR{Number: t.PR.Number, URL: t.PR.URL, State: "merged"}
		}
	default:
		return p, false
	}
	return p, true
}

// ---- import from GitHub ----

// GitHubIssue is an issue of a repository.
type GitHubIssue struct {
	Number int
	Title  string
	Body   string
	URL    string
	Labels []string
	Author string
}

// OpenIssues lists the open issues (not pull requests) of owner/repo with an installation token limited to the
// repository, optionally only those with label; at most max.
func (g *GitHubApp) OpenIssues(ctx context.Context, installation int64, owner, repo, label string, max int) ([]GitHubIssue, error) {
	tok, err := g.InstallationToken(ctx, installation, repo)
	if err != nil {
		return nil, err
	}
	var out []GitHubIssue
	for page := 1; len(out) < max && page <= 10; page++ {
		q := url.Values{"state": {"open"}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if label != "" {
			q.Set("labels", label)
		}
		var items []struct {
			Number      int             `json:"number"`
			Title       string          `json:"title"`
			Body        string          `json:"body"`
			HTMLURL     string          `json:"html_url"`
			PullRequest json.RawMessage `json:"pull_request"`
			User        struct {
				Login string `json:"login"`
			} `json:"user"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
		}
		path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/issues?" + q.Encode()
		if _, err := g.do(ctx, http.MethodGet, path, tok, nil, &items); err != nil {
			return nil, err
		}
		for _, it := range items {
			if len(it.PullRequest) > 0 { // the issues API lists pull requests too
				continue
			}
			is := GitHubIssue{Number: it.Number, Title: it.Title, Body: it.Body, URL: it.HTMLURL, Author: it.User.Login}
			for _, l := range it.Labels {
				is.Labels = append(is.Labels, l.Name)
			}
			if out = append(out, is); len(out) >= max {
				break
			}
		}
		if len(items) < 100 {
			break
		}
	}
	return out, nil
}

func (s *server) importRequirements(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	var b struct {
		Label string `json:"label"`
	}
	if !decode(w, r, &b) {
		return
	}
	owner, repo, parsed := ParseRepo(p.RepoURL)
	switch {
	case s.opt.GitHub == nil:
		writeError(w, http.StatusConflict, "github_not_configured", "This server has no GitHub App configured.")
		return
	case !parsed || p.GitHubInstallation == 0:
		writeError(w, http.StatusConflict, "github_not_connected", "Connect the project's GitHub repository first.")
		return
	}
	issues, err := s.opt.GitHub.OpenIssues(r.Context(), p.GitHubInstallation, owner, repo, strings.TrimSpace(b.Label), 300)
	if err != nil {
		slog.Warn("requirements: listing GitHub issues failed", "project", p.ID, "error", err)
		writeError(w, http.StatusBadGateway, "github_error", "GitHub could not be reached; try again.")
		return
	}
	out := RequirementImport{Requirements: []requirements.Requirement{}}
	for _, is := range issues {
		created, isNew, err := s.opt.Requirements.Create(r.Context(), requirements.Requirement{
			ProjectID: p.ID, Title: is.Title, Body: is.Body, Labels: is.Labels, Author: c.Actor,
			Source: requirements.Origin{Kind: requirements.SourceGitHubIssue, Repo: owner + "/" + repo, Number: is.Number, URL: is.URL},
		})
		if errors.Is(err, requirements.ErrInvalid) { // an issue without a title
			out.Skipped++
			continue
		}
		if err != nil {
			fail(w, err)
			return
		}
		if !isNew {
			out.Skipped++
			continue
		}
		out.Imported++
		out.Requirements = append(out.Requirements, *created)
	}
	s.note(inProject(c, p.ID), "", "requirement.import", map[string]any{"imported": out.Imported, "skipped": out.Skipped}, FeedEvent{Type: "requirement.changed"})
	writeJSON(w, http.StatusOK, out)
}
