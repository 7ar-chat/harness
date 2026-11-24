package product

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/schedule"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/hosts/client"
	"github.com/7ar-chat/harness/hosts/inproc"
	"github.com/7ar-chat/harness/modelinfo"
)

// Scheduled agent tasks: a prompt the agent runs by itself on a cron or interval schedule, in a project (or for one
// person), as an ordinary agent run in a conversation the project's members can open.
//
//	GET    /api/projects/{pid}/schedules               → [Schedule]
//	POST   /api/projects/{pid}/schedules {name,prompt,cron,timezone,model,policy,timeoutMinutes,reuseSession,enabled} → 201 Schedule   (member)
//	GET    /api/projects/{pid}/schedules/{sid}         → Schedule
//	PATCH  /api/projects/{pid}/schedules/{sid} {any of the fields}  → Schedule   (member)
//	DELETE /api/projects/{pid}/schedules/{sid}         → 204   (member)
//	POST   /api/projects/{pid}/schedules/{sid}/run     → 202 ScheduleRun   (member; 409 while a run is in progress)
//	GET    /api/projects/{pid}/schedules/{sid}/runs?limit= → [ScheduleRun]
//	GET    /api/me/schedules, POST /api/me/schedules, GET|PATCH|DELETE /api/me/schedules/{sid},
//	POST /api/me/schedules/{sid}/run, GET /api/me/schedules/{sid}/runs      the same for the caller's personal schedules
//	GET    /api/schedules/preview?cron=&timezone=&count=   → {times:[...]}   the next fire times, for the editor
//
// Who the run is: the account that last saved the schedule (RunAs). The server sets it from the session, a client cannot
// send one, and the run is skipped when that account no longer has write access to the project. A fire missed while the
// server was down is skipped, never replayed; a schedule runs once at a time (a tick that finds the previous run still
// going is recorded as skipped); a run still going after its timeout is cancelled. Several replicas may serve the same
// store: the store hands each due tick to one of them.

// SchedulesOptions turns scheduled tasks on.
type SchedulesOptions struct {
	Store schedule.Store
	// Locker, when set, keeps all but one replica from claiming on each tick (Redis); the store is correct without it.
	Locker harness.ScheduleLocker
	// Every is how often due schedules are looked for (0: 15s).
	Every time.Duration
	// Price prices the usage of a run for its history (nil: tokens only, the cost is flagged unpriced).
	Price func(model string) (modelinfo.Info, bool)
	// Context ends the scheduler when it is done (nil: it runs for the life of the process).
	Context context.Context
	// Timeout overrides the time limit of a run (nil: the schedule's own; for tests).
	Timeout func(schedule.Schedule) time.Duration
	// NoRun serves the routes without firing anything in this process (a replica that only answers requests).
	NoRun bool
}

// maxSchedules bounds the schedules of a project or a person.
const maxSchedules = 50

// scheduleView is a schedule with its last run, for lists.
type scheduleView struct {
	schedule.Schedule
	LastRun *schedule.Run `json:"lastRun,omitempty"`
	// CanEdit is whether the caller may change, run or delete it.
	CanEdit bool `json:"canEdit"`
}

type scheduleBody struct {
	Name           *string `json:"name"`
	Prompt         *string `json:"prompt"`
	Cron           *string `json:"cron"`
	Timezone       *string `json:"timezone"`
	Model          *string `json:"model"`
	Policy         *string `json:"policy"`
	TimeoutMinutes *int    `json:"timeoutMinutes"`
	ReuseSession   *bool   `json:"reuseSession"`
	Enabled        *bool   `json:"enabled"`
}

func (b scheduleBody) apply(s *schedule.Schedule) {
	if b.Name != nil {
		s.Name = *b.Name
	}
	if b.Prompt != nil {
		s.Prompt = *b.Prompt
	}
	if b.Cron != nil {
		s.Cron = *b.Cron
	}
	if b.Timezone != nil {
		s.Timezone = *b.Timezone
	}
	if b.Model != nil {
		s.Model = strings.TrimSpace(*b.Model)
	}
	if b.Policy != nil {
		s.Policy = *b.Policy
	}
	if b.TimeoutMinutes != nil {
		s.TimeoutMinutes = *b.TimeoutMinutes
	}
	if b.ReuseSession != nil {
		s.ReuseSession = *b.ReuseSession
	}
	if b.Enabled != nil {
		s.Enabled = *b.Enabled
	}
}

// schedulerState is what the server keeps of the scheduler.
type schedulerState struct {
	svc *harness.ScheduleService
}

func (s *server) routeSchedules() {
	o := s.opt.Schedules
	if o == nil || o.Store == nil {
		return
	}
	s.sched = &schedulerState{svc: harness.NewScheduleService(harness.ScheduleOptions{
		Store: o.Store, Exec: &scheduleExecutor{s: s}, Every: o.Every, Locker: o.Locker, ID: "web", Timeout: o.Timeout,
		// The schedules list follows a run to its end without waiting for a poll.
		OnRunFinished: func(sc schedule.Schedule, _ schedule.Run) {
			s.hub.publish(FeedEvent{Type: "schedule.updated", ProjectID: sc.ProjectID, Title: sc.Name, owner: sc.RunAs})
		},
	})}
	// A deleted conversation is no longer the reused session of a schedule: its next fire starts a fresh one.
	s.H.OnThreadDeleted(func(ctx context.Context, threadID string) {
		if err := o.Store.ClearThread(ctx, threadID); err != nil {
			slog.Warn("schedules: forgetting the reused session of a deleted thread failed", "thread", threadID, "error", err)
		}
	})
	if !o.NoRun {
		ctx := o.Context
		if ctx == nil {
			ctx = context.Background()
		}
		go func() { _ = s.sched.svc.Run(ctx) }()
	}
	s.Route("GET /projects/{pid}/schedules", s.listSchedules)
	s.Route("POST /projects/{pid}/schedules", s.createSchedule)
	s.Route("GET /projects/{pid}/schedules/{sid}", s.getSchedule)
	s.Route("PATCH /projects/{pid}/schedules/{sid}", s.patchSchedule)
	s.Route("DELETE /projects/{pid}/schedules/{sid}", s.deleteSchedule)
	s.Route("POST /projects/{pid}/schedules/{sid}/run", s.runSchedule)
	s.Route("GET /projects/{pid}/schedules/{sid}/runs", s.scheduleRuns)
	s.Route("GET /me/schedules", s.listSchedules)
	s.Route("POST /me/schedules", s.createSchedule)
	s.Route("GET /me/schedules/{sid}", s.getSchedule)
	s.Route("PATCH /me/schedules/{sid}", s.patchSchedule)
	s.Route("DELETE /me/schedules/{sid}", s.deleteSchedule)
	s.Route("POST /me/schedules/{sid}/run", s.runSchedule)
	s.Route("GET /me/schedules/{sid}/runs", s.scheduleRuns)
	s.Route("GET /schedules/preview", s.previewSchedule)
}

// dropProjectSchedules removes the schedules of a deleted project: nothing may keep firing into it.
func (s *server) dropProjectSchedules(ctx context.Context, projectID string) {
	if s.sched == nil {
		return
	}
	list, err := s.opt.Schedules.Store.List(ctx, projectID, "")
	if err != nil {
		slog.Warn("schedules: listing the schedules of a deleted project failed", "project", projectID, "error", err)
		return
	}
	for _, sc := range list {
		if err := s.opt.Schedules.Store.Delete(ctx, sc.ID); err != nil && !errors.Is(err, schedule.ErrNotFound) {
			slog.Warn("schedules: deleting the schedule of a deleted project failed", "schedule", sc.ID, "error", err)
		}
	}
}

// schedScope resolves the project (or the person) a schedule route is about and checks the caller's access: need is
// the project role required. The project id is "" for the personal routes.
func (s *server) schedScope(w http.ResponseWriter, r *http.Request, c Caller, need string) (projectID string, ok bool) {
	pid := r.PathValue("pid")
	if pid == "" { // /me/schedules: the caller's own
		return "", true
	}
	if _, err := s.requireProject(r.Context(), pid, c.Actor, need); err != nil {
		s.FailAccess(w, err)
		return "", false
	}
	return pid, true
}

// schedOf loads the schedule of the path and checks it belongs to the scope.
func (s *server) schedOf(w http.ResponseWriter, r *http.Request, c Caller, projectID string) (*schedule.Schedule, bool) {
	sc, err := s.opt.Schedules.Store.Get(r.Context(), r.PathValue("sid"))
	if errors.Is(err, schedule.ErrNotFound) || err == nil && (sc.ProjectID != projectID || projectID == "" && sc.Owner != c.Actor) {
		s.FailAccess(w, errNotFound)
		return nil, false
	}
	if err != nil {
		fail(w, err)
		return nil, false
	}
	return sc, true
}

func scheduleFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, schedule.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_schedule", strings.TrimPrefix(err.Error(), "schedule: invalid: "))
	case errors.Is(err, schedule.ErrBusy):
		writeError(w, http.StatusConflict, "run_in_progress", "This schedule already has a run in progress.")
	case errors.Is(err, schedule.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "Not found.")
	default:
		fail(w, err)
	}
}

func (s *server) viewOf(ctx context.Context, sc schedule.Schedule, canEdit bool) scheduleView {
	v := scheduleView{Schedule: sc, CanEdit: canEdit}
	if runs, err := s.opt.Schedules.Store.Runs(ctx, sc.ID, 1); err == nil && len(runs) > 0 {
		v.LastRun = &runs[0]
	}
	return v
}

func (s *server) canWrite(ctx context.Context, c Caller, projectID string) bool {
	if projectID == "" {
		return true
	}
	role, err := s.team.MemberRole(ctx, projectID, c.Actor)
	return err == nil && store.RoleAtLeast(role, store.RoleMember)
}

func (s *server) listSchedules(w http.ResponseWriter, r *http.Request, c Caller) {
	pid, ok := s.schedScope(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	list, err := s.opt.Schedules.Store.List(r.Context(), pid, c.Actor)
	if err != nil {
		scheduleFail(w, err)
		return
	}
	can := s.canWrite(r.Context(), c, pid)
	out := make([]scheduleView, 0, len(list))
	for _, sc := range list {
		out = append(out, s.viewOf(r.Context(), sc, can))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) createSchedule(w http.ResponseWriter, r *http.Request, c Caller) {
	pid, ok := s.schedScope(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	var b scheduleBody
	if !decode(w, r, &b) {
		return
	}
	existing, err := s.opt.Schedules.Store.List(r.Context(), pid, c.Actor)
	if err != nil {
		scheduleFail(w, err)
		return
	}
	if len(existing) >= maxSchedules {
		writeError(w, http.StatusBadRequest, "invalid_schedule", fmt.Sprintf("At most %d schedules.", maxSchedules))
		return
	}
	// The identity is the server's: the owner and the account the runs execute as are the session's, whatever the body says.
	sc := schedule.Schedule{ProjectID: pid, Owner: c.Actor, RunAs: c.Actor, Enabled: true}
	b.apply(&sc)
	if !s.checkModel(w, sc.Model) || !s.prepare(w, &sc) {
		return
	}
	out, err := s.opt.Schedules.Store.Create(r.Context(), sc)
	if err != nil {
		scheduleFail(w, err)
		return
	}
	s.note(inProject(c, pid), "", "schedule.create", map[string]any{"schedule": out.ID, "name": out.Name}, FeedEvent{Type: "schedule.updated", Title: out.Name})
	writeJSON(w, http.StatusCreated, s.viewOf(r.Context(), *out, true))
}

// checkModel refuses a model the server does not serve, when the schedule is saved rather than when it first fires.
func (s *server) checkModel(w http.ResponseWriter, model string) bool {
	if model == "" || len(s.O.Models) == 0 || model == s.O.DefaultModel || slices.Contains(s.O.Models, model) {
		return true
	}
	writeError(w, http.StatusBadRequest, "invalid_schedule", fmt.Sprintf("Model %q is not available.", model))
	return false
}

func (s *server) prepare(w http.ResponseWriter, sc *schedule.Schedule) bool {
	if err := s.sched.svc.Prepare(sc); err != nil {
		scheduleFail(w, err)
		return false
	}
	return true
}

func (s *server) getSchedule(w http.ResponseWriter, r *http.Request, c Caller) {
	pid, ok := s.schedScope(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	sc, ok := s.schedOf(w, r, c, pid)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.viewOf(r.Context(), *sc, s.canWrite(r.Context(), c, pid)))
}

func (s *server) patchSchedule(w http.ResponseWriter, r *http.Request, c Caller) {
	pid, ok := s.schedScope(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	sc, ok := s.schedOf(w, r, c, pid)
	if !ok {
		return
	}
	var b scheduleBody
	if !decode(w, r, &b) {
		return
	}
	b.apply(sc)
	sc.RunAs = c.Actor // whoever saved it last is who it runs as
	if !s.checkModel(w, sc.Model) || !s.prepare(w, sc) {
		return
	}
	out, err := s.opt.Schedules.Store.Update(r.Context(), *sc)
	if err != nil {
		scheduleFail(w, err)
		return
	}
	s.note(inProject(c, pid), "", "schedule.update", map[string]any{"schedule": out.ID, "name": out.Name}, FeedEvent{Type: "schedule.updated", Title: out.Name})
	writeJSON(w, http.StatusOK, s.viewOf(r.Context(), *out, true))
}

func (s *server) deleteSchedule(w http.ResponseWriter, r *http.Request, c Caller) {
	pid, ok := s.schedScope(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	sc, ok := s.schedOf(w, r, c, pid)
	if !ok {
		return
	}
	if err := s.opt.Schedules.Store.Delete(r.Context(), sc.ID); err != nil {
		scheduleFail(w, err)
		return
	}
	s.note(inProject(c, pid), "", "schedule.delete", map[string]any{"schedule": sc.ID, "name": sc.Name}, FeedEvent{Type: "schedule.updated", Title: sc.Name})
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) runSchedule(w http.ResponseWriter, r *http.Request, c Caller) {
	pid, ok := s.schedScope(w, r, c, store.RoleMember)
	if !ok {
		return
	}
	sc, ok := s.schedOf(w, r, c, pid)
	if !ok {
		return
	}
	run, err := s.sched.svc.RunNow(r.Context(), sc.ID, c.Actor)
	if err != nil {
		scheduleFail(w, err)
		return
	}
	s.note(inProject(c, pid), "", "schedule.run", map[string]any{"schedule": sc.ID, "name": sc.Name}, FeedEvent{Type: "schedule.updated", Title: sc.Name})
	writeJSON(w, http.StatusAccepted, run)
}

func (s *server) scheduleRuns(w http.ResponseWriter, r *http.Request, c Caller) {
	pid, ok := s.schedScope(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	sc, ok := s.schedOf(w, r, c, pid)
	if !ok {
		return
	}
	runs, err := s.opt.Schedules.Store.Runs(r.Context(), sc.ID, min(limitParam(r, 50), 200))
	if err != nil {
		scheduleFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

// schedulePreview answers the editor's "next fire times".
type schedulePreview struct {
	Times []time.Time `json:"times"`
}

func (s *server) previewSchedule(w http.ResponseWriter, r *http.Request, _ Caller) {
	q := r.URL.Query()
	n, _ := strconv.Atoi(q.Get("count"))
	if n <= 0 || n > 20 {
		n = 5
	}
	times, err := harness.PreviewSchedule(q.Get("cron"), q.Get("timezone"), n, time.Now())
	if err != nil {
		scheduleFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, schedulePreview{Times: times})
}

// ---- the run ----

// scheduleExecutor runs a schedule's prompt as an ordinary agent run of its RunAs account.
type scheduleExecutor struct{ s *server }

// Execute implements harness.ScheduleExecutor.
func (e *scheduleExecutor) Execute(ctx context.Context, sc schedule.Schedule, run schedule.Run, started func(threadID, agentRunID string)) (harness.ScheduleOutcome, error) {
	s := e.s
	who := sc.RunAs
	if u, err := s.team.GetUser(ctx, who); err == nil && !u.Active() {
		return harness.ScheduleOutcome{Status: schedule.StatusSkipped, Error: who + "'s account is not active"}, nil
	}
	if sc.ProjectID != "" {
		role, err := s.team.MemberRole(ctx, sc.ProjectID, who)
		if err != nil || !store.RoleAtLeast(role, store.RoleMember) {
			return harness.ScheduleOutcome{Status: schedule.StatusSkipped,
				Error: who + " can no longer write to this project; save the schedule again to run it as someone else"}, nil
		}
	}
	changed := func() {
		s.hub.publish(FeedEvent{Type: "schedule.updated", ProjectID: sc.ProjectID, Title: sc.Name, Actor: who, owner: who})
	}
	owner, threadID := who, ""
	if sc.ReuseSession && sc.ThreadID != "" {
		if meta, err := s.team.GetThreadMeta(ctx, sc.ThreadID); err == nil && meta.ProjectID == sc.ProjectID && meta.Kind == store.ThreadChat {
			owner, threadID = meta.Owner, sc.ThreadID
		}
	}
	cl := inproc.New(s.H, owner, inproc.WithModels(s.O.Models, s.O.DefaultModel),
		inproc.WithAuthor(harness.Author{Login: "schedule", Kind: store.AuthorSystem, OnBehalfOf: who}))
	if threadID == "" {
		title := sc.Name + " · " + run.StartedAt.Format("2006-01-02 15:04")
		th, err := cl.CreateThread(ctx, title)
		if err != nil {
			return harness.ScheduleOutcome{}, fmt.Errorf("creating the conversation: %w", err)
		}
		threadID = th.ID
		meta := store.ThreadMeta{ThreadID: th.ID, ProjectID: sc.ProjectID, Owner: owner, CreatedBy: who, Kind: store.ThreadChat}
		if err := s.team.SetThreadMeta(ctx, meta); err != nil {
			_, _ = s.H.DeleteThread(context.WithoutCancel(ctx), owner, th.ID)
			return harness.ScheduleOutcome{}, fmt.Errorf("linking the conversation to its project: %w", err)
		}
		s.note(Caller{Login: owner, Actor: who, ProjectID: sc.ProjectID}, th.ID, "thread.create", map[string]any{"schedule": sc.ID}, FeedEvent{Type: "thread.created", Title: title})
	}
	started(threadID, "")
	changed()
	opts := client.SendOptions{Model: sc.Model}
	if sc.Policy == schedule.PolicyReadOnly {
		opts.Mode = client.ModePlan
	}
	agentRun, err := cl.Send(ctx, threadID, sc.Prompt, opts)
	if err != nil {
		return harness.ScheduleOutcome{ThreadID: threadID}, err
	}
	started(threadID, agentRun.ID)
	s.AfterRun(Caller{Login: owner, Actor: who, ProjectID: sc.ProjectID}, agentRun, "schedule.run", map[string]any{"schedule": sc.ID, "run": run.ID})
	out := harness.ScheduleOutcome{ThreadID: threadID, AgentRunID: agentRun.ID}
	last, ok := e.wait(ctx, owner, agentRun.ID)
	if !ok {
		return out, ctx.Err()
	}
	out.Status, out.Error, out.Usage = e.outcome(last, agentRun.Model)
	return out, nil
}

// wait follows the agent run to its terminal event. When ctx ends first the run is cancelled and its end is awaited a
// moment, so the conversation is left finished and its usage is known.
func (e *scheduleExecutor) wait(ctx context.Context, owner, runID string) (store.Event, bool) {
	var last store.Event
	for ev := range e.s.H.Events(ctx, runID, 0) {
		last = ev
		if agui.IsTerminal(ev.Type) {
			return last, true
		}
	}
	if ctx.Err() == nil {
		return last, false
	}
	base := context.WithoutCancel(ctx)
	if err := e.s.H.Cancel(base, owner, runID); err != nil {
		slog.Warn("schedules: cancelling a run that timed out failed", "run", runID, "error", err)
	}
	wctx, cancel := context.WithTimeout(base, 30*time.Second)
	defer cancel()
	for ev := range e.s.H.Events(wctx, runID, 0) {
		if agui.IsTerminal(ev.Type) {
			return ev, false
		}
	}
	return last, false
}

// outcome reads what the terminal event of a run says.
func (e *scheduleExecutor) outcome(ev store.Event, fallbackModel string) (status, msg string, u schedule.RunUsage) {
	var body struct {
		Message string `json:"message"`
		Outcome struct {
			Type string `json:"type"`
		} `json:"outcome"`
		Usage []agui.Usage `json:"usage"`
	}
	_ = json.Unmarshal(ev.Data, &body)
	for _, m := range body.Usage {
		if m.InputTokens == 0 && m.OutputTokens == 0 {
			continue
		}
		model := m.Model
		if model == "" {
			model = fallbackModel
		}
		usd, unpriced := harness.RunCost(m, model, e.s.opt.Schedules.Price)
		u.InputTokens += int64(m.InputTokens)
		u.OutputTokens += int64(m.OutputTokens)
		u.CacheReadTokens += int64(m.CachedInputTokens)
		u.CacheWriteTokens += int64(m.CacheWriteTokens)
		u.CostUSD += usd
		if unpriced {
			u.Unpriced++
		}
	}
	switch {
	case ev.Type == agui.RunError:
		return schedule.StatusFailed, firstNonEmpty(body.Message, "the run failed"), u
	case body.Outcome.Type == "cancelled":
		return schedule.StatusCancelled, "the run was cancelled", u
	case body.Outcome.Type == "interrupt":
		return schedule.StatusFailed, "the agent stopped to ask for an approval or an answer; scheduled runs are unattended", u
	}
	return schedule.StatusSucceeded, "", u
}
