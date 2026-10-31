package runtime

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

// Process claims a run and executes it synchronously. A run that is no longer
// queued is ignored (nil error).
func (r *Runtime) Process(ctx context.Context, runID string) error {
	run, err := r.store.ClaimRun(ctx, runID, r.opts.ID)
	if errors.Is(err, store.ErrNotClaimable) {
		return nil
	}
	if err != nil {
		return err
	}
	r.wg.Add(1)
	defer r.wg.Done()
	r.execute(ctx, run)
	return nil
}

// Run consumes the queue until ctx is done, then drains in-flight runs: they
// get ShutdownGrace to finish, then are cancelled and finished as interrupted.
func (r *Runtime) Run(ctx context.Context) error {
	for {
		if err := r.q.EnsureGroup(ctx); err == nil {
			break
		} else {
			slog.Error("runtime: cannot create consumer group", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
	r.lastRead.Store(time.Now().UnixNano())
	r.consuming.Store(true)
	defer r.consuming.Store(false)
	go r.sweepLoop(ctx)
	slog.Info("runtime started", "runtime_id", r.opts.ID, "concurrency", r.opts.Concurrency)
	for ctx.Err() == nil {
		select {
		case r.sem <- struct{}{}:
		case <-ctx.Done():
			continue
		}
		msgs, err := r.q.Read(ctx, r.opts.ID, 1, r.opts.ReadBlock)
		if err == nil {
			r.lastRead.Store(time.Now().UnixNano())
		}
		if err != nil || len(msgs) == 0 {
			<-r.sem
			if err != nil && ctx.Err() == nil {
				slog.Warn("runtime: queue read failed", "error", err)
				time.Sleep(time.Second)
			}
			continue
		}
		m := msgs[0]
		run, err := r.store.ClaimRun(ctx, m.RunID, r.opts.ID)
		if aerr := r.q.Ack(context.WithoutCancel(ctx), m.StreamID); aerr != nil {
			slog.Warn("runtime: ack failed", "error", aerr)
		}
		if err != nil {
			<-r.sem
			if !errors.Is(err, store.ErrNotClaimable) {
				slog.Error("runtime: claim failed", "run_id", m.RunID, "error", err)
			}
			continue
		}
		r.wg.Add(1)
		go func() {
			defer func() { <-r.sem; r.wg.Done() }()
			r.execute(ctx, run)
		}()
	}
	r.drain()
	return nil
}

func (r *Runtime) drain() {
	// Interrupted runs get InterruptWait to write RUN_ERROR; every finishing
	// retry stops by then (minus a margin to close the log).
	r.stopBy.Store(time.Now().Add(r.opts.ShutdownGrace + r.opts.InterruptWait - r.opts.InterruptWait/5).UnixNano())
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		return
	case <-time.After(r.opts.ShutdownGrace):
	}
	r.mu.Lock()
	n := len(r.inflight)
	for _, cancel := range r.inflight {
		cancel(errShutdown)
	}
	r.mu.Unlock()
	slog.Warn("runtime: interrupting in-flight runs", "count", n)
	select {
	case <-done:
	case <-time.After(r.opts.InterruptWait):
		slog.Error("runtime: in-flight runs did not stop in time")
	}
}
