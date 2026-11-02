package runtime

import (
	"context"
	"log/slog"
	"time"
)

// Job is a periodic maintenance job run by the sweeper under a Redis lock
// (work:job:<Name>:lock, held for Every), like the retention purge.
type Job struct {
	Name  string
	Every time.Duration
	Run   func(ctx context.Context) error
}

// Purger hard-deletes data past its retention.
type Purger interface {
	Purge(ctx context.Context) error
}

// runJobs runs the periodic jobs that are due (across runtimes when Locks is set).
func (r *Runtime) runJobs(ctx context.Context) {
	for _, j := range r.opts.Jobs {
		if j.Run == nil || j.Every <= 0 {
			continue
		}
		r.mu.Lock()
		if r.lastJob == nil {
			r.lastJob = map[string]time.Time{}
		}
		last, ran := r.lastJob[j.Name]
		due := !ran || time.Since(last) >= j.Every
		if due {
			r.lastJob[j.Name] = time.Now()
		}
		r.mu.Unlock()
		if !due {
			continue
		}
		if r.locks != nil {
			if ok, err := r.locks.SetNX(ctx, "work:job:"+j.Name+":lock", r.opts.ID, j.Every); err != nil || !ok {
				continue
			}
		}
		jctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		if err := j.Run(jctx); err != nil {
			slog.Warn("sweeper: periodic job failed", "job", j.Name, "error", err)
		}
		cancel()
	}
}

// purge runs the retention job at most every PurgeEvery (across runtimes when Locks is set).
func (r *Runtime) purge(ctx context.Context) {
	if r.opts.Purger == nil {
		return
	}
	r.mu.Lock()
	due := r.lastPurge.IsZero() || time.Since(r.lastPurge) >= r.opts.PurgeEvery
	if due {
		r.lastPurge = time.Now()
	}
	r.mu.Unlock()
	if !due {
		return
	}
	if r.locks != nil {
		if ok, err := r.locks.SetNX(ctx, "work:purge:lock", r.opts.ID, r.opts.PurgeEvery); err != nil || !ok {
			return
		}
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := r.opts.Purger.Purge(pctx); err != nil {
		slog.Warn("sweeper: retention purge failed", "error", err)
	}
}
