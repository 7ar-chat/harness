package sandboxtools

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/sandbox"
)

// Background jobs keep their sandbox alive (harness v3 §2, stream A's
// sandbox.KeepAlive): a job still running when its execute_command returns
// renews the sandbox lease until it ends, is killed, its sandbox is gone or
// Options.JobKeepAliveMax passes. The job's status is polled every
// Options.JobPollEvery, so the lease ends with the job even when the model
// never asks for it again (or asks on another replica). Foreground commands
// and job_output waits need nothing: Exec of a manager-issued sandbox renews
// it while the command runs.

const (
	defaultJobPollEvery    = time.Minute
	defaultJobKeepAliveMax = 6 * time.Hour
)

// jobKeeper holds the keep-alives of the running background jobs; list
// (Options.Jobs) persists them for a restarted process (ResumeJobs) and gives
// each job one owner (a lease), so replicas never keep the same job alive twice.
type jobKeeper struct {
	mu    sync.Mutex
	stops map[string]func() // threadID "/" jobID → stop
	list  JobList
	owner string // this process (the lease holder id)
}

func jobKey(threadID, jobID string) string { return threadID + "/" + jobID }

// leaseTTL is how long a job's lease outlives its owner's last renewal.
func leaseTTL(opts Options) time.Duration { return 3 * opts.JobPollEvery }

// keep renews sb while the job runs, until its start + JobKeepAliveMax
// (started: the job's start, zero for a job that starts now); running reports
// whether it still does (false ends the keep-alive; an error keeps it, unless
// the sandbox is gone). With a list, the job is recorded (its first start is
// kept) and leased to this process; a lease lost to another process ends the
// keep-alive here without forgetting the job.
func (k *jobKeeper) keep(ctx context.Context, opts Options, sb sandbox.Sandbox, threadID, jobID string, started time.Time, running func(context.Context) (bool, error)) {
	key := jobKey(threadID, jobID)
	k.mu.Lock()
	if k.stops[key] != nil {
		k.mu.Unlock()
		return // already kept by this process
	}
	k.mu.Unlock()
	if started.IsZero() {
		started = time.Now()
	}
	if k.list != nil {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statusTimeout)
		first, err := k.list.Add(actx, threadID, jobID)
		if err == nil {
			started = first
			if ok, cerr := k.list.Claim(actx, threadID, jobID, k.owner, leaseTTL(opts)); cerr == nil && !ok {
				cancel()
				return // another process keeps it alive
			}
		} else {
			slog.Warn("sandboxtools: recording a running job failed; a restart will not resume its keep-alive", "thread_id", threadID, "job_id", jobID, "error", err)
		}
		cancel()
	}
	kctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), started.Add(opts.JobKeepAliveMax))
	renew := opts.KeepAlive(kctx, sb)
	var lost atomic.Bool // the lease went to another process: the job stays listed
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			renew()
			k.mu.Lock()
			delete(k.stops, key)
			k.mu.Unlock()
			if k.list != nil && !lost.Load() {
				rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), statusTimeout)
				defer rcancel()
				_ = k.list.Remove(rctx, threadID, jobID)
				_ = k.list.Release(rctx, threadID, jobID, k.owner)
			}
		})
	}
	k.mu.Lock()
	if k.stops == nil {
		k.stops = map[string]func(){}
	}
	k.stops[key] = stop
	k.mu.Unlock()
	go func() {
		defer stop()
		t := time.NewTicker(opts.JobPollEvery)
		defer t.Stop()
		for {
			select {
			case <-kctx.Done():
				return
			case <-t.C:
				if k.list != nil {
					if ok, err := k.list.Claim(kctx, threadID, jobID, k.owner, leaseTTL(opts)); err == nil && !ok {
						lost.Store(true)
						return
					}
				}
				ok, err := running(kctx)
				if (err == nil && !ok) || sandbox.IsGone(err) {
					return
				}
			}
		}
	}()
}

// release ends the keep-alive of a job that finished or was killed.
func (k *jobKeeper) release(threadID, jobID string) {
	k.mu.Lock()
	stop := k.stops[jobKey(threadID, jobID)]
	k.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// kept reports whether a job's sandbox is being kept alive (tests).
func (k *jobKeeper) kept(threadID, jobID string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.stops[jobKey(threadID, jobID)] != nil
}

// ListedJob is a running background job of the JobList.
type ListedJob struct {
	ThreadID, JobID string
	// Started is when the job started (its keep-alive budget counts from it).
	Started time.Time
}

// JobList persists the running background jobs so that a restarted runtime
// process resumes their keep-alives (ResumeJobs), and leases each job to one
// process at a time.
type JobList interface {
	// Add records a running job and returns its start: the first one recorded.
	Add(ctx context.Context, threadID, jobID string) (time.Time, error)
	Remove(ctx context.Context, threadID, jobID string) error
	List(ctx context.Context) ([]ListedJob, error)
	// Claim takes the job's lease for owner, or renews it when owner holds it
	// (false: another process holds it).
	Claim(ctx context.Context, threadID, jobID, owner string, ttl time.Duration) (bool, error)
	// Release drops owner's lease.
	Release(ctx context.Context, threadID, jobID, owner string) error
}

// ResumeJobs re-registers the keep-alive of every listed job still running in
// its thread's live sandbox that no other process keeps alive (a runtime
// process start, then periodically: KeepResumingJobs), for what is left of its
// budget; jobs past their budget, finished or without a sandbox are dropped
// from the list. It creates no sandbox.
func (p *Provider) ResumeJobs(ctx context.Context) int {
	list := p.jobs.list
	if list == nil {
		return 0
	}
	jobs, err := list.List(ctx)
	if err != nil {
		slog.Warn("sandboxtools: listing the running jobs failed", "error", err)
		return 0
	}
	resumed := 0
	for _, j := range jobs {
		threadID, jobID := j.ThreadID, j.JobID
		if p.jobs.kept(threadID, jobID) {
			continue
		}
		if time.Since(j.Started) >= p.opts.JobKeepAliveMax {
			_ = list.Remove(ctx, threadID, jobID) // its keep-alive budget is spent
			continue
		}
		if ok, err := list.Claim(ctx, threadID, jobID, p.jobs.owner, leaseTTL(p.opts)); err != nil || !ok {
			continue // kept alive by another process (or Redis is unavailable now)
		}
		sb, err := p.mgr.Get(ctx, threadID)
		if errors.Is(err, sandbox.ErrNoSandbox) {
			_ = list.Remove(ctx, threadID, jobID) // its sandbox is gone, and the job with it
			_ = list.Release(ctx, threadID, jobID, p.jobs.owner)
			continue
		}
		if err != nil {
			_ = list.Release(ctx, threadID, jobID, p.jobs.owner)
			continue // unavailable now: tried again later
		}
		b := &binding{p: p, rc: tools.RunContext{ThreadID: threadID}}
		dir := jobDir(jobID)
		if st, err := b.status(ctx, sb, dir, false); err != nil || st.status != "running" {
			if err == nil {
				_ = list.Remove(ctx, threadID, jobID)
			}
			_ = list.Release(ctx, threadID, jobID, p.jobs.owner)
			continue
		}
		p.jobs.keep(ctx, p.opts, sb, threadID, jobID, j.Started, func(ctx context.Context) (bool, error) {
			st, err := b.status(ctx, sb, dir, false)
			return st.status == "running", err
		})
		resumed++
	}
	return resumed
}

// KeepResumingJobs runs ResumeJobs now and then every `every` until ctx ends:
// a job whose owner died (its lease expired) is taken over by a live replica.
func (p *Provider) KeepResumingJobs(ctx context.Context, every time.Duration, done func(resumed int)) {
	for {
		rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		n := p.ResumeJobs(rctx)
		cancel()
		if done != nil {
			done(n)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
