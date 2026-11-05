package harness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/runtime"
	"github.com/7ar-chat/harness/features/jobhunt"
)

// JobSkill is the skill the autonomous search asks for (shipped with the product, see app/product/skills).
const JobSkill = "job-search"

// maxJobRunsPerDay bounds how often the search wakes the agent for one user.
const maxJobRunsPerDay = 12

// Jobs is the job search: each user's profile and jobs under HARNESS_JOBS_DIR, the jobs_* tools, and, for the users who
// turned it on, a periodic scan of their job boards that wakes the agent when there is something to judge or apply to.
// Without HARNESS_JOBS_DIR it contributes nothing.
func Jobs() Feature {
	return builtin{FeatureJobs, func(b *Builder) error {
		cfg := b.Config
		if cfg.JobsDir == "" {
			return nil
		}
		svc := jobhunt.New(cfg.JobsDir)
		b.Jobs = svc
		b.AddTools(jobhunt.Provider{Svc: svc})
		b.Section(func(_ context.Context, in agent.Input) string {
			if in.Run == nil {
				return ""
			}
			return jobSection(svc, in.Run.UserLogin)
		})
		if cfg.JobsScanMinutes <= 0 {
			return nil
		}
		var h *Harness
		b.OnReady(func(x *Harness) { h = x })
		w := &jobWorker{svc: svc, model: cfg.LLMDefaultModel, now: time.Now}
		b.Job(runtime.Job{Name: "jobhunt", Every: time.Duration(cfg.JobsScanMinutes) * time.Minute, Run: func(ctx context.Context) error {
			if h == nil {
				return nil
			}
			w.h = h
			return w.run(ctx)
		}})
		return nil
	}}
}

// jobSection tells the agent about the person's search when they have one.
func jobSection(svc *jobhunt.Service, login string) string {
	p, err := svc.Profile(login)
	if err != nil || (len(p.Titles) == 0 && len(p.Companies) == 0 && !p.Enabled) {
		return "<job_search>\nYou can run a job search for the user (jobs_* tools): they tell you the roles, places and limits once and you keep them in their profile (jobs_profile_set); the skill job-search says how to search, judge and apply.\n</job_search>"
	}
	counts, _ := svc.Counts(login)
	var b strings.Builder
	fmt.Fprintf(&b, "<job_search>\nThe user is looking for work. Mode: %s", p.Mode)
	if p.Enabled {
		b.WriteString(", running by itself")
	}
	fmt.Fprintf(&b, ". Roles: %s. Jobs: %d new, %d waiting for the user, %d approved, %d applied.", strings.Join(p.Titles, ", "),
		counts[jobhunt.StatusNew], counts[jobhunt.StatusQueued], counts[jobhunt.StatusApproved], counts[jobhunt.StatusApplied])
	b.WriteString(" jobs_profile has the details and the limits. Use the job-search skill to search, judge and apply, and never apply without jobs_claim.\n</job_search>")
	return b.String()
}

// jobWorker is the periodic part: scan the boards of every user who turned the search on, and when there is work for the
// agent, start a run in the user's conversation of the day.
type jobWorker struct {
	svc   *jobhunt.Service
	h     *Harness
	model string
	now   func() time.Time
}

func (w *jobWorker) run(ctx context.Context) error {
	users, err := w.svc.Users()
	if err != nil {
		return err
	}
	for _, login := range users {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := w.one(ctx, login); err != nil {
			slog.Warn("jobhunt: user run failed", "err", err)
		}
	}
	return nil
}

func (w *jobWorker) one(ctx context.Context, login string) error {
	p, err := w.svc.Profile(login)
	if err != nil || !p.Enabled {
		return err
	}
	if len(p.Titles) > 0 && len(p.Companies) > 0 {
		if res, err := w.svc.Scan(ctx, login); err != nil {
			slog.Warn("jobhunt: scan failed", "err", err)
		} else if len(res.Errors) > 0 {
			slog.Info("jobhunt: some boards failed", "errors", len(res.Errors))
		}
	}
	counts, err := w.svc.Counts(login)
	if err != nil {
		return err
	}
	todo := counts[jobhunt.StatusNew] + counts[jobhunt.StatusApproved]
	if todo == 0 {
		return nil
	}
	st, err := w.svc.RunState(login)
	if err != nil {
		return err
	}
	day := w.now().UTC().Format("2006-01-02")
	if st.Day != day || st.ThreadID == "" {
		t, err := w.h.NewThread(ctx, login, "Job search "+day)
		if err != nil {
			return err
		}
		st = jobhunt.RunState{Day: day, ThreadID: t.ID}
	}
	if st.Runs >= maxJobRunsPerDay {
		return nil
	}
	msg := fmt.Sprintf("Scheduled job-search round (nobody is watching this conversation; the person reads the result later). "+
		"There are %d new jobs to judge and %d approved ones to apply to. Follow the job-search skill: read jobs_profile, judge the new jobs, "+
		"apply only to approved ones through jobs_claim, and finish with a short summary of what you did and what needs the person.",
		counts[jobhunt.StatusNew], counts[jobhunt.StatusApproved])
	if _, err := w.h.Send(ctx, login, st.ThreadID, msg, RunOptions{Model: w.model, Skills: []string{JobSkill}}); err != nil {
		if errors.Is(err, store.ErrRunActive) { // the last round is still working: the next one will see its results
			return w.svc.SetRunState(login, st)
		}
		return err
	}
	st.Runs++
	return w.svc.SetRunState(login, st)
}
