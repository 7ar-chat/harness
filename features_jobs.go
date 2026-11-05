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
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/jobhunt"
	"github.com/7ar-chat/harness/prompts"
)

// maxJobRunsPerDay bounds how often the search wakes the agent for one user.
const maxJobRunsPerDay = 12

// JobThreadTitle is the title of a user's job-search conversation.
const JobThreadTitle = "Job search"

// Jobs is the job search, a mode of its own next to coding. Each user has one job-search conversation; in it the agent is the
// job-application agent (the jobs prompt, the jobs_* tools, no code explorer), everywhere else it is whatever the deployment's
// profile says. The feature also keeps each user's profile and jobs under HARNESS_JOBS_DIR and, for the users who turned it
// on, scans their job boards every HARNESS_JOBS_SCAN_MINUTES and wakes the agent in that conversation when there is
// something to do. Without HARNESS_JOBS_DIR it contributes nothing.
func Jobs() Feature {
	return builtin{FeatureJobs, func(b *Builder) error {
		cfg := b.Config
		if cfg.JobsDir == "" {
			return nil
		}
		svc := jobhunt.New(cfg.JobsDir)
		b.Jobs = svc
		b.AddTools(jobhunt.Provider{Svc: svc})
		b.WrapTools(func(inner tools.Provider) tools.Provider {
			return tools.Filter{Inner: inner, Allow: func(_ context.Context, rc tools.RunContext, name string) bool {
				inJobs := svc.IsJobThread(rc.Login, rc.ThreadID)
				switch {
				case strings.HasPrefix(name, "jobs_"):
					return inJobs // the job tools exist only in the job-search conversation
				case name == "explore":
					return !inJobs // the code explorer has nothing to read there
				}
				return true
			}}
		})
		b.RunVariant(func(_ context.Context, run *store.Run) string {
			if run != nil && svc.IsJobThread(run.UserLogin, run.ThreadID) {
				return prompts.Jobs
			}
			return ""
		})
		b.Section(func(_ context.Context, in agent.Input) string {
			if in.Run == nil {
				return ""
			}
			return jobSection(svc, in.Run.UserLogin, svc.IsJobThread(in.Run.UserLogin, in.Run.ThreadID))
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

// JobThread returns the user's job-search conversation, creating it the first time (or when it was deleted). Runs in it are
// in job mode. It fails when the harness has no job search.
func (h *Harness) JobThread(ctx context.Context, login string) (string, error) {
	if h.Jobs == nil {
		return "", errors.New("this harness has no job search")
	}
	st, err := h.Jobs.RunState(login)
	if err != nil {
		return "", err
	}
	if st.ThreadID != "" {
		if _, err := h.Store.GetThread(ctx, login, st.ThreadID); err == nil {
			return st.ThreadID, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
	}
	t, err := h.NewThread(ctx, login, JobThreadTitle)
	if err != nil {
		return "", err
	}
	st.ThreadID = t.ID
	return t.ID, h.Jobs.SetRunState(login, st)
}

// jobSection tells the agent about the person's search. In the job-search conversation it carries the state; elsewhere it
// only says where the job search lives, because the jobs_* tools are not offered there.
func jobSection(svc *jobhunt.Service, login string, inJobs bool) string {
	if !inJobs {
		return "<job_search>\nThe user has a job-search mode with its own conversation (the Jobs page in the app opens it). If they ask you to find or apply for jobs here, tell them to open it; do not try to do it in this conversation.\n</job_search>"
	}
	p, err := svc.Profile(login)
	if err != nil || (len(p.Titles) == 0 && len(p.Companies) == 0 && !p.Enabled) {
		return "<job_search>\nThe person has not set up their search yet: start by asking for what jobs_profile lacks.\n</job_search>"
	}
	counts, _ := svc.Counts(login)
	var b strings.Builder
	fmt.Fprintf(&b, "<job_search>\nMode: %s", p.Mode)
	if p.Enabled {
		b.WriteString(", running by itself")
	}
	fmt.Fprintf(&b, ". Roles: %s. Jobs: %d new, %d waiting for the person, %d approved, %d applied.\n</job_search>", strings.Join(p.Titles, ", "),
		counts[jobhunt.StatusNew], counts[jobhunt.StatusQueued], counts[jobhunt.StatusApproved], counts[jobhunt.StatusApplied])
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
	threadID, err := w.h.JobThread(ctx, login)
	if err != nil {
		return err
	}
	st, err := w.svc.RunState(login)
	if err != nil {
		return err
	}
	st.ThreadID = threadID
	day := w.now().UTC().Format("2006-01-02")
	if st.Day != day {
		st.Day, st.Runs = day, 0
	}
	if st.Runs >= maxJobRunsPerDay {
		return nil
	}
	msg := fmt.Sprintf("Scheduled round (nobody is watching right now; the person reads the result later). "+
		"There are %d new jobs to judge and %d approved ones to apply to. Do a round as described: read jobs_profile, judge the new jobs, "+
		"apply only to approved ones through jobs_claim, and finish with a short summary of what you did and what needs the person.",
		counts[jobhunt.StatusNew], counts[jobhunt.StatusApproved])
	if _, err := w.h.Send(ctx, login, st.ThreadID, msg, RunOptions{Model: w.model}); err != nil {
		if errors.Is(err, store.ErrRunActive) { // the last round is still working: the next one will see its results
			return nil
		}
		return err
	}
	st.Runs++
	return w.svc.SetRunState(login, st)
}
