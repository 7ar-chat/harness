package product

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/insights"
	"github.com/7ar-chat/harness/core/requirements"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/core/usage"
	"github.com/7ar-chat/harness/core/workflows"
)

// Insights: a few plain answers about what the agents delivered and what it cost (the numbers are defined in
// core/insights).
//
//	GET /api/projects/{pid}/insights?range=7d|30d|90d|FROM..TO&tz=Europe/Berlin           → insights.Result   (project viewer+)
//	GET /api/projects/{pid}/insights/report?range=&tz=&format=html|csv|json[&download=1]   → the exportable report
//	GET /api/me/insights?range=&tz=                                                        → the caller's own work across projects
//	GET /api/threads/{id}/usage                                                            → what one conversation cost
//
// Ranges are cut in the time zone tz (default UTC) and are at most a year. The report is a self-contained HTML page
// (inline CSS and SVG, no script, no request), the task table as CSV, or the raw numbers as JSON.

const maxTasksWithHistory = 1500

func (s *server) routeInsights() {
	if s.opt.Usage == nil && s.factory() == nil {
		return
	}
	s.Route("GET /projects/{pid}/insights", s.projectInsights)
	s.Route("GET /projects/{pid}/insights/report", s.projectInsightsReport)
	s.Route("GET /me/insights", s.myInsights)
	if s.opt.Usage != nil && s.opt.Usage.Store != nil {
		s.Route("GET /threads/{id}/usage", s.threadUsage)
	}
}

func (s *server) insightsNow() time.Time {
	if s.opt.Usage != nil {
		return s.opt.Usage.now()
	}
	return time.Now().UTC()
}

// insightsRange resolves range and tz against now: whole days in the zone, today included.
func insightsRange(rng, tz string, now time.Time) (from, to time.Time, loc *time.Location, err error) {
	loc = time.UTC
	if tz != "" {
		if loc, err = time.LoadLocation(tz); err != nil {
			return from, to, nil, fmt.Errorf("tz must be an IANA time zone such as Europe/Berlin")
		}
	}
	local := now.In(loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	if rng == "" {
		rng = "30d"
	}
	if strings.HasSuffix(rng, "d") && !strings.Contains(rng, "..") {
		var n int
		if _, e := fmt.Sscanf(rng, "%dd", &n); e != nil || n < 1 || n > insights.MaxDays {
			return from, to, nil, fmt.Errorf("range must be 7d, 30d, 90d (1d to %dd) or FROM..TO", insights.MaxDays)
		}
		return today.AddDate(0, 0, -(n - 1)), today.AddDate(0, 0, 1), loc, nil
	}
	a, b, found := strings.Cut(rng, "..")
	if !found {
		return from, to, nil, fmt.Errorf("range must be 7d, 30d, 90d or FROM..TO")
	}
	fa, ok1, aDate := parseWhen(a)
	fb, ok2, bDate := parseWhen(b)
	if !ok1 || !ok2 {
		return from, to, nil, fmt.Errorf("range dates must be YYYY-MM-DD or RFC3339")
	}
	if aDate { // a date means midnight in the caller's zone
		fa = time.Date(fa.Year(), fa.Month(), fa.Day(), 0, 0, 0, 0, loc)
	}
	if bDate {
		fb = time.Date(fb.Year(), fb.Month(), fb.Day()+1, 0, 0, 0, 0, loc)
	}
	if !fb.After(fa) || fb.Sub(fa) > time.Duration(insights.MaxDays+1)*24*time.Hour {
		return from, to, nil, fmt.Errorf("range must be between one second and %d days", insights.MaxDays)
	}
	return fa, fb, loc, nil
}

func (s *server) insightsParams(w http.ResponseWriter, r *http.Request) (from, to time.Time, loc *time.Location, ok bool) {
	q := r.URL.Query()
	from, to, loc, err := insightsRange(q.Get("range"), q.Get("tz"), s.insightsNow())
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return from, to, nil, false
	}
	return from, to, loc, true
}

// compute loads what the numbers need and computes them. projectID "" with user set is the personal view.
func (s *server) computeInsights(ctx context.Context, projectID, user string, from, to time.Time, loc *time.Location) (insights.Result, error) {
	in := insights.Input{From: from, To: to, Now: s.insightsNow(), Loc: loc}
	if f := s.factory(); f != nil {
		filter := factory.Filter{ProjectID: projectID, Owner: user, Limit: 1 << 20}
		ts, err := f.Tasks.List(ctx, filter)
		if err != nil {
			return insights.Result{}, err
		}
		n := 0
		for _, t := range ts {
			// Only tasks that could matter get their history loaded: created in the range, or touched since it began.
			if t.CreatedAt.After(to) || (t.UpdatedAt.Before(from) && t.CreatedAt.Before(from)) {
				continue
			}
			if n++; n > maxTasksWithHistory {
				break
			}
			evs, err := f.Tasks.Events(ctx, t.ID, 0)
			if err != nil {
				return insights.Result{}, err
			}
			in.Tasks = append(in.Tasks, insights.TaskInput{Task: t, Events: evs})
		}
	}
	if projectID != "" {
		if s.opt.Requirements != nil {
			reqs, err := s.opt.Requirements.List(ctx, requirements.Filter{ProjectID: projectID, Limit: 5000})
			if err != nil {
				return insights.Result{}, err
			}
			in.Requirements = reqs
		}
		if s.opt.Workflows != nil {
			runs, err := s.opt.Workflows.ListRuns(ctx, workflows.RunFilter{ProjectID: projectID, Limit: 1000})
			if err != nil {
				return insights.Result{}, err
			}
			in.Runs = runs
		}
		audit, err := s.team.ListAudit(ctx, projectID, 5000)
		if err != nil {
			return insights.Result{}, err
		}
		in.Audit = audit
	}
	if s.opt.Usage != nil && s.opt.Usage.Store != nil {
		in.HasLedger = true
		st := s.usageStore()
		base := usage.Query{ProjectID: projectID, User: user, From: from, To: to, Loc: loc}
		for _, g := range []struct {
			group string
			into  func(*usage.Report)
		}{
			{"", func(r *usage.Report) { in.UsageTotal = r.Total }},
			{usage.GroupModel, func(r *usage.Report) { in.UsageByModel = r.Rows }},
			{usage.GroupUser, func(r *usage.Report) { in.UsageByUser = r.Rows }},
			{usage.GroupDayModel, func(r *usage.Report) { in.UsageDayModel = r.Rows }},
		} {
			q := base
			q.Group = g.group
			rep, err := st.Report(ctx, q)
			if err != nil {
				return insights.Result{}, err
			}
			g.into(rep)
		}
	}
	res := insights.Compute(in)
	res.ProjectID = projectID
	return res, nil
}

func (s *server) projectInsights(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	from, to, loc, ok := s.insightsParams(w, r)
	if !ok {
		return
	}
	res, err := s.computeInsights(r.Context(), p.ID, "", from, to, loc)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// myInsights is the caller's own work: the tasks they own and the runs booked to them, in every project.
func (s *server) myInsights(w http.ResponseWriter, r *http.Request, c Caller) {
	from, to, loc, ok := s.insightsParams(w, r)
	if !ok {
		return
	}
	res, err := s.computeInsights(r.Context(), "", c.Actor, from, to, loc)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// threadUsageView answers GET /api/threads/{id}/usage.
type threadUsageView struct {
	ThreadID string `json:"threadId"`
	// TaskID is set when the conversation is a factory task's.
	TaskID  string            `json:"taskId,omitempty"`
	Total   usage.UsageTotals `json:"total"`
	ByModel []usage.UsageRow  `json:"byModel"`
}

func (s *server) threadUsage(w http.ResponseWriter, r *http.Request, c Caller) {
	id := r.PathValue("id")
	tc, err := s.access(r.Context(), c.Actor, id, store.RoleViewer)
	if err != nil {
		s.FailAccess(w, err)
		return
	}
	if _, err := s.H.Store.GetThread(r.Context(), tc.Login, id); err != nil {
		s.FailAccess(w, errNotFound)
		return
	}
	st := s.usageStore()
	tot, err := st.Report(r.Context(), usage.Query{ThreadID: id})
	if err != nil {
		fail(w, err)
		return
	}
	by, err := st.Report(r.Context(), usage.Query{ThreadID: id, Group: usage.GroupModel})
	if err != nil {
		fail(w, err)
		return
	}
	v := threadUsageView{ThreadID: id, Total: tot.Total, ByModel: by.Rows}
	if f := s.factory(); f != nil {
		if ti, err := f.Store.GetThreadInfo(r.Context(), id); err == nil && ti != nil {
			v.TaskID = ti.TaskID
		}
	}
	writeJSON(w, http.StatusOK, v)
}
