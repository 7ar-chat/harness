package product

import (
	"context"
	"errors"
	"github.com/7ar-chat/harness"
	"net/http"
	"strings"
	"time"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/schedule"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/core/usage"
)

// Usage and spend caps. Every finished run is booked in a ledger (features/usageledger, written by an end-of-run hook
// the command installs); this serves it back, priced, and keeps the monthly caps.
//
//	GET /api/usage?scope=me|project&project=ID&range=day|week|month|FROM..TO&group=day|model|thread|user → UsageView
//	PUT /api/me/usage/cap              {monthlyUsd}   the caller's own monthly cap (0 removes it)
//	PUT /api/projects/{pid}/usage/cap  {monthlyUsd}   the project's monthly cap (owner; 0 removes it)
//
// Ranges: day is the last 24 hours, week the last 7 days, month the calendar month so far (the window the caps count in,
// UTC), FROM..TO two dates (YYYY-MM-DD, the last one included) or RFC3339 instants. A run that would start while its
// project's or its user's cap is used up is refused with 402 spend_cap_reached; the page warns from 80%.

// UsageOptions turns the usage routes on.
type UsageOptions struct {
	// Store is the ledger and the caps (the same the harness's end-of-run hook and run gate use).
	Store usage.Store
	// Now is the clock (nil: time.Now).
	Now func() time.Time
}

func (o *UsageOptions) now() time.Time {
	if o != nil && o.Now != nil {
		return o.Now().UTC()
	}
	return time.Now().UTC()
}

// UsageAttribution says whom a run's cost is booked to: the project of its conversation, the person who started it,
// and whether it is a task or a scheduled run. It is what the command hands to features/usageledger.
func UsageAttribution(team store.TeamStore, sched schedule.Store, tasks factory.Store) func(ctx context.Context, run *store.Run) harness.UsageAttribution {
	return func(ctx context.Context, run *store.Run) harness.UsageAttribution {
		a := harness.UsageAttribution{User: run.UserLogin, Source: usage.SourceSession}
		if team != nil {
			if meta, err := team.GetThreadMeta(ctx, run.ThreadID); err == nil {
				a.ProjectID = meta.ProjectID
				if meta.CreatedBy != "" {
					a.User = meta.CreatedBy
				}
			}
		}
		if sched != nil {
			if r, err := sched.RunOfThread(ctx, run.ThreadID); err == nil {
				a.Source, a.SourceID = usage.SourceSchedule, r.ScheduleID
				return a
			}
		}
		if tasks != nil {
			if ti, err := tasks.GetThreadInfo(ctx, run.ThreadID); err == nil && ti != nil && ti.TaskID != "" {
				a.Source, a.SourceID = usage.SourceTask, ti.TaskID
			}
		}
		return a
	}
}

// UsageGateResolver is the attribution of a run about to start, for features/usageledger.Gate.
func UsageGateResolver(team store.TeamStore) func(ctx context.Context, user, threadID string) (string, string) {
	return func(ctx context.Context, _, threadID string) (string, string) {
		if team == nil {
			return "", ""
		}
		meta, err := team.GetThreadMeta(ctx, threadID)
		if err != nil {
			return "", ""
		}
		return meta.ProjectID, meta.CreatedBy
	}
}

type capView struct {
	// MonthlyUSD is the cap (0: none); SpentUSD what this month's runs cost so far; Ratio their quotient (0 without a cap).
	MonthlyUSD float64 `json:"monthlyUsd"`
	SpentUSD   float64 `json:"spentUsd"`
	Ratio      float64 `json:"ratio"`
	// Warn is true from 80% of the cap, Reached once it is used up: new runs are then refused.
	Warn    bool `json:"warn"`
	Reached bool `json:"reached"`
}

// usageView answers GET /api/usage.
type usageView struct {
	Scope     string `json:"scope"`
	ProjectID string `json:"projectId,omitempty"`
	// From and To bound the range (To is exclusive).
	From  time.Time         `json:"from"`
	To    time.Time         `json:"to"`
	Group string            `json:"group"`
	Total usage.UsageTotals `json:"total"`
	// BySource splits the total between interactive sessions, factory tasks and scheduled runs.
	BySource map[string]usage.UsageTotals `json:"bySource"`
	Rows     []usage.UsageRow             `json:"rows"`
	// Titles names the threads of a thread grouping (by id).
	Titles map[string]string `json:"titles,omitempty"`
	// Cap is the monthly cap of the scope (the project's, or the caller's own) with this month's spend.
	Cap capView `json:"cap"`
	// CanSetCap says whether the caller may change the cap (a project's owner; anyone for their own).
	CanSetCap bool `json:"canSetCap"`
}

func (s *server) routeUsage() {
	if s.opt.Usage == nil || s.opt.Usage.Store == nil {
		return
	}
	s.Route("GET /usage", s.getUsage)
	s.Route("PUT /me/usage/cap", s.putMyCap)
	s.Route("PUT /projects/{pid}/usage/cap", s.putProjectCap)
}

func (s *server) usageStore() usage.Store { return s.opt.Usage.Store }

// usageRange resolves the range parameter against now.
func usageRange(v string, now time.Time) (from, to time.Time, ok bool) {
	switch v {
	case "", "month":
		return usage.MonthStart(now), now.Add(time.Second), true
	case "day":
		return now.Add(-24 * time.Hour), now.Add(time.Second), true
	case "week":
		return now.Add(-7 * 24 * time.Hour), now.Add(time.Second), true
	}
	a, b, found := strings.Cut(v, "..")
	if !found {
		return from, to, false
	}
	from, ok1, _ := parseWhen(a)
	to, ok2, dateOnly := parseWhen(b)
	if !ok1 || !ok2 {
		return from, to, false
	}
	if dateOnly { // "2026-11-01..2026-11-07" includes the 7th
		to = to.Add(24 * time.Hour)
	}
	return from, to, to.After(from) && to.Sub(from) <= 3660*24*time.Hour
}

func parseWhen(v string) (t time.Time, ok, dateOnly bool) {
	v = strings.TrimSpace(v)
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t.UTC(), true, true
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), true, false
	}
	return time.Time{}, false, false
}

func (s *server) getUsage(w http.ResponseWriter, r *http.Request, c Caller) {
	q := r.URL.Query()
	now := s.opt.Usage.now()
	from, to, ok := usageRange(q.Get("range"), now)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "range must be day, week, month or FROM..TO (two dates, at most ten years apart).")
		return
	}
	group := q.Get("group")
	if group == "" {
		group = usage.GroupDay
	}
	if !usage.ValidGroup(group) {
		writeError(w, http.StatusBadRequest, "invalid_request", "group must be day, model, thread or user.")
		return
	}
	scope := q.Get("scope")
	view := usageView{Scope: scope, From: from, To: to, Group: group}
	query := usage.Query{From: from, To: to, Group: group}
	var capScope, capID string
	switch scope {
	case "", "me":
		view.Scope = "me"
		query.User = c.Actor
		capScope, capID, view.CanSetCap = usage.ScopeUser, c.Actor, true
	case "project":
		pid := q.Get("project")
		role, err := s.requireProject(r.Context(), pid, c.Actor, store.RoleViewer)
		if err != nil {
			s.FailAccess(w, err)
			return
		}
		view.ProjectID, query.ProjectID = pid, pid
		capScope, capID, view.CanSetCap = usage.ScopeProject, pid, store.RoleAtLeast(role, store.RoleOwner)
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "scope must be me or project.")
		return
	}
	rep, err := s.usageStore().Report(r.Context(), query)
	if err != nil {
		fail(w, err)
		return
	}
	view.Total, view.BySource, view.Rows = rep.Total, rep.BySource, rep.Rows
	if view.Cap, err = s.capOf(r.Context(), capScope, capID, now); err != nil {
		fail(w, err)
		return
	}
	if group == usage.GroupThread {
		view.Titles = s.threadTitles(r.Context(), rep.Rows)
	}
	writeJSON(w, http.StatusOK, view)
}

// capOf is the cap of a scope with the spend of the calendar month of now.
func (s *server) capOf(ctx context.Context, scope, id string, now time.Time) (capView, error) {
	st := s.usageStore()
	limit, err := st.Cap(ctx, scope, id)
	if err != nil {
		return capView{}, err
	}
	var spent float64
	if scope == usage.ScopeProject {
		spent, err = st.Spent(ctx, id, "", usage.MonthStart(now))
	} else {
		spent, err = st.Spent(ctx, "", id, usage.MonthStart(now))
	}
	if err != nil {
		return capView{}, err
	}
	v := capView{MonthlyUSD: limit, SpentUSD: spent}
	if limit > 0 {
		v.Ratio = spent / limit
		v.Warn, v.Reached = v.Ratio >= usage.WarnAt, spent >= limit
	}
	return v, nil
}

// threadTitles names the threads of the rows the caller may see: the report is already limited to their project or to
// their own work, so only the title lookup (stored under the thread's owner) is needed.
func (s *server) threadTitles(ctx context.Context, rows []usage.UsageRow) map[string]string {
	out := map[string]string{}
	for _, row := range rows {
		meta, err := s.team.GetThreadMeta(ctx, row.Key)
		if err != nil {
			continue
		}
		if th, err := s.H.Store.GetThread(ctx, meta.Owner, row.Key); err == nil {
			out[row.Key] = th.Title
		}
	}
	return out
}

type capBody struct {
	MonthlyUSD float64 `json:"monthlyUsd"`
}

func (s *server) putCap(w http.ResponseWriter, r *http.Request, c Caller, scope, id string) {
	var body capBody
	if !decode(w, r, &body) {
		return
	}
	if body.MonthlyUSD < 0 || body.MonthlyUSD > 1e9 {
		writeError(w, http.StatusBadRequest, "invalid_request", "monthlyUsd must be between 0 and 1000000000 (0 removes the cap).")
		return
	}
	if err := s.usageStore().SetCap(r.Context(), scope, id, body.MonthlyUSD); err != nil {
		if errors.Is(err, usage.ErrInvalid) {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		fail(w, err)
		return
	}
	pid := ""
	if scope == usage.ScopeProject {
		pid = id
	}
	s.note(Caller{Login: c.Actor, Actor: c.Actor, ProjectID: pid}, "", "usage.cap", map[string]any{"scope": scope, "monthlyUsd": body.MonthlyUSD}, FeedEvent{Type: "usage.cap"})
	v, err := s.capOf(r.Context(), scope, id, s.opt.Usage.now())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) putMyCap(w http.ResponseWriter, r *http.Request, c Caller) {
	s.putCap(w, r, c, usage.ScopeUser, c.Actor)
}

func (s *server) putProjectCap(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleOwner)
	if !ok {
		return
	}
	s.putCap(w, r, c, usage.ScopeProject, p.ID)
}
