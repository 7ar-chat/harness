// Package runtime claims queued runs, drives the agent and finishes runs.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/agent"
	"github.com/GhaziBenDahmane/harness/agui"
	"github.com/GhaziBenDahmane/harness/eventlog"
	"github.com/GhaziBenDahmane/harness/queue"
	"github.com/GhaziBenDahmane/harness/store"
)

var (
	// ErrCancelled is the cancel cause of a run cancelled by the user.
	ErrCancelled = errors.New("run cancelled by the user")
	errShutdown  = errors.New("runtime shutting down")
	errLost      = errors.New("run ownership lost")
)

// InterruptedMessage is the RUN_ERROR message of interrupted runs.
const InterruptedMessage = "The run was interrupted (the runtime stopped). Send your message again to continue."

// Executor runs the agent loop of one run.
type Executor interface {
	Run(ctx context.Context, in agent.Input, w eventlog.Writer) (agent.Result, error)
}

// RunQueue is the queue side used by the runtime.
type RunQueue interface {
	Enqueue(ctx context.Context, runID string) error
	EnsureGroup(ctx context.Context) error
	Read(ctx context.Context, consumer string, count int, block time.Duration) ([]queue.Message, error)
	Ack(ctx context.Context, streamID string) error
	Reclaim(ctx context.Context, consumer string, minIdle time.Duration) ([]queue.Message, error)
	PruneConsumers(ctx context.Context, keep string, idle time.Duration) (int, error)
}

// Options tunes the runtime.
type Options struct {
	ID string
	// Reporter receives internal errors and panics (an error tracker such as Sentry); nil: logs only.
	Reporter      ErrorReporter
	Concurrency   int
	Heartbeat     time.Duration // default 10 s
	SweepEvery    time.Duration // default 30 s
	QueuedAge     time.Duration // default 30 s
	StaleAge      time.Duration // default 60 s
	ShutdownGrace time.Duration // default 15 s: in-flight runs may finish normally
	InterruptWait time.Duration // default 8 s: then interrupted runs get this long to write RUN_ERROR
	FinishRetry   time.Duration // default 30 s: retry budget for recording the final status
	ReadBlock     time.Duration // default 2 s (bounds shutdown latency: go-redis ignores ctx cancel while blocked)
	// ConsumerIdle: queue consumers idle for longer, without pending entries,
	// are deleted by the sweeper (default 1 h); entries pending for longer than
	// QueuedAge on a consumer are reclaimed.
	ConsumerIdle time.Duration
	// StuckAfter: Health fails when the queue loop has not completed a read for
	// that long while it has free slots (default 60 s, at least 5 × ReadBlock).
	StuckAfter time.Duration
	// Hooks run at the end of each run, in order, before its terminal event
	// (end-of-run artifact snapshot first, then titles, goals).
	Hooks []Hook
	// Purger enforces the retention (purge of deleted threads and their blobs),
	// run by the sweeper at most every PurgeEvery (default 1 h) across runtimes.
	Purger     Purger
	PurgeEvery time.Duration
	// Jobs are periodic maintenance jobs of the sweeper (memory consolidation),
	// each run at most every Job.Every across runtimes.
	Jobs []Job
}

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

// Runtime is a run worker.
type Runtime struct {
	opts  Options
	store store.Store
	log   eventlog.Log
	q     RunQueue
	bus   queue.Bus
	exec  Executor
	rdb   redis.UniversalClient

	mu        sync.Mutex
	inflight  map[string]context.CancelCauseFunc
	lastPurge time.Time
	lastJob   map[string]time.Time
	wg        sync.WaitGroup
	sem       chan struct{}
	// Health of the queue loop: consuming once the group exists, lastRead is
	// the unix nanos of the last completed queue read.
	consuming atomic.Bool
	lastRead  atomic.Int64
	// stopBy (unix nanos, 0 = not stopping) is when a shutdown must be done:
	// finishing runs bound their retries by it (B-11: the pod is SIGKILLed).
	stopBy atomic.Int64
}

// finishDeadline is when a run's finishing retries (status, steers, log) must
// stop: FinishRetry from now, or the shutdown deadline if earlier.
func (r *Runtime) finishDeadline(budget time.Duration) time.Time {
	d := time.Now().Add(budget)
	if s := r.stopBy.Load(); s != 0 && time.Unix(0, s).Before(d) {
		return time.Unix(0, s)
	}
	return d
}

// finishCtx bounds the run's event-log close by the shutdown deadline.
func (r *Runtime) finishCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithDeadline(context.WithoutCancel(ctx), r.finishDeadline(r.opts.FinishRetry))
}

// Health reports whether the runtime consumes its queue: an error when the
// consumer group could not be created yet, or when the queue loop has free
// slots but no read completed for StuckAfter (stuck connection, Redis down).
func (r *Runtime) Health() error {
	if !r.consuming.Load() {
		return errors.New("queue consumer group not ready")
	}
	if len(r.sem) == cap(r.sem) {
		return nil // every slot runs a run: not reading is expected
	}
	if since := time.Since(time.Unix(0, r.lastRead.Load())); since > r.opts.StuckAfter {
		return fmt.Errorf("queue loop stuck: no queue read completed for %s", since.Round(time.Second))
	}
	return nil
}

// New builds a runtime. rdb is used for the sweeper lock (optional).
func New(opts Options, st store.Store, log eventlog.Log, q RunQueue, bus queue.Bus, exec Executor, rdb redis.UniversalClient) *Runtime {
	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&opts.Heartbeat, 10*time.Second)
	def(&opts.SweepEvery, 30*time.Second)
	def(&opts.QueuedAge, 30*time.Second)
	def(&opts.StaleAge, 60*time.Second)
	def(&opts.ShutdownGrace, 15*time.Second)
	def(&opts.InterruptWait, 8*time.Second)
	def(&opts.FinishRetry, 30*time.Second)
	def(&opts.ReadBlock, 2*time.Second)
	def(&opts.ConsumerIdle, time.Hour)
	def(&opts.StuckAfter, max(time.Minute, 5*opts.ReadBlock))
	def(&opts.PurgeEvery, time.Hour)
	return &Runtime{opts: opts, store: st, log: log, q: q, bus: bus, exec: exec, rdb: rdb,
		inflight: map[string]context.CancelCauseFunc{}, sem: make(chan struct{}, opts.Concurrency)}
}

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

func (r *Runtime) execute(parent context.Context, run *store.Run) {
	runCtx, cancel := context.WithCancelCause(context.WithoutCancel(parent))
	defer cancel(nil)
	r.mu.Lock()
	r.inflight[run.ID] = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.inflight, run.ID)
		r.mu.Unlock()
	}()
	log := slog.With("run_id", run.ID, "thread_id", run.ThreadID)
	log.Info("run started")

	if r.bus != nil {
		unsub, err := r.bus.SubscribeCancel(runCtx, run.ID, func() { cancel(ErrCancelled) })
		if err != nil {
			log.Warn("cancel subscription failed; relying on heartbeat polling", "error", err)
		}
		defer unsub()
	}
	hbDone := make(chan struct{})
	defer close(hbDone)
	go r.heartbeat(run, cancel, hbDone)
	if run.CancelRequested {
		cancel(ErrCancelled)
	}

	w := &ownedWriter{Writer: r.log.Writer(run.ID, run.LastSeq)}
	var res agent.Result
	var err error
	if _, aerr := w.Append(runCtx, agui.RunStartedEvent(run.ThreadID, run.ID)); aerr != nil {
		err = &agent.Error{Code: agui.CodeInternal, Message: agent.MsgInternal, Detail: aerr.Error()}
	} else {
		res, err = r.safeRun(runCtx, agent.Input{Run: run}, w)
	}

	if w.lost.Load() { // another writer (the sweeper) appended to this run's log
		cancel(errLost)
	}
	status, errMsg, final := r.classify(runCtx, run, res, err)
	if final == nil { // ownership lost: the sweeper finished this run
		r.abandon(runCtx, run, w)
		return
	}
	// Stop accepting steers; accepted but undelivered steers stay in the thread and are announced.
	announceSteers(runCtx, w, r.keepUndeliveredSteers(runCtx, run))
	r.recordInterrupt(runCtx, run, status, res)
	finish := &Finish{Run: run, Status: status, Result: res, w: w}
	after := r.runHooks(runCtx, finish)
	final = finish.withUsage(final) // the hooks' model calls count in RUN_FINISHED.usage (harness v3 4.5)
	// Persist, then finish the run, then publish the terminal event: a client that
	// sees RUN_FINISHED and reloads the thread must find the run terminal.
	if ferr := w.Flush(runCtx); ferr != nil {
		log.Error("flushing events failed", "error", ferr)
	}
	// The row turns terminal before the continuations emit their last events:
	// replay waits for the real terminal event meanwhile (never a synthetic one).
	unmark := r.log.Finalizing(runCtx, run.ID, r.opts.FinishRetry+2*time.Minute)
	defer unmark()
	if !r.finishRun(run, status, errorCode(final), errMsg) || w.lost.Load() {
		r.abandon(runCtx, run, w)
		return
	}
	for _, next := range after {
		safeHook(run.ID, func() struct{} { next(runCtx); return struct{}{} })
	}
	if _, aerr := w.Append(runCtx, final); aerr != nil {
		log.Error("writing terminal event failed", "error", aerr)
	}
	cctx, ccancel := r.finishCtx(runCtx)
	defer ccancel()
	if cerr := w.Close(cctx); cerr != nil {
		log.Error("closing event log failed", "error", cerr)
	}
	log.Info("run finished", "status", status)
}

// ownedWriter records that an append found its seq written by another writer
// (the sweeper finished the run): the run's ownership is lost.
type ownedWriter struct {
	eventlog.Writer
	lost atomic.Bool
}

func (o *ownedWriter) Append(ctx context.Context, ev agui.Event) (int64, error) {
	seq, err := o.Writer.Append(ctx, ev)
	if errors.Is(err, eventlog.ErrConflict) {
		o.lost.Store(true)
	}
	return seq, err
}

// abandon stops a run another runtime (the sweeper) finished: no hooks'
// continuations, no terminal event; what was written is persisted.
func (r *Runtime) abandon(ctx context.Context, run *store.Run, w eventlog.Writer) {
	slog.Warn("run ownership lost; stopping without writing a terminal event", "run_id", run.ID)
	ctx, cancel := r.finishCtx(ctx)
	defer cancel()
	if cerr := w.Close(ctx); cerr != nil { // persist what we have, expire the stream
		slog.Warn("closing event log failed", "run_id", run.ID, "error", cerr)
	}
}

// finishRun records the final status within FinishRetry (survives short
// Postgres outages); false when the run is no longer this runtime's.
func (r *Runtime) finishRun(run *store.Run, status, code, errMsg string) bool {
	deadline := r.finishDeadline(r.opts.FinishRetry)
	for backoff := 100 * time.Millisecond; ; backoff = min(backoff*2, 2*time.Second) {
		actx, acancel := context.WithTimeout(context.Background(), 5*time.Second)
		ferr := r.store.FinishRun(actx, run.ID, r.opts.ID, status, code, errMsg)
		if errors.Is(ferr, store.ErrNotOwned) {
			// A retry of an update whose reply was lost finds its own result.
			cur, gerr := r.store.GetRun(actx, run.ID)
			ferr = gerr
			if gerr == nil {
				acancel()
				return cur.RuntimeID == r.opts.ID && cur.Status == status
			}
		}
		acancel()
		if ferr == nil {
			return true
		}
		if time.Now().Add(backoff).After(deadline) {
			slog.Error("finishing run failed; the sweeper will reconcile it from the log", "run_id", run.ID, "error", ferr)
			return true
		}
		time.Sleep(backoff)
	}
}

// keepUndeliveredSteers closes the run's steer list and stores leftovers as
// thread messages; it returns the stored ones (to announce). Steers it cannot
// store stay pending in Redis and are stored by a later sweep (sweepSteers).
func (r *Runtime) keepUndeliveredSteers(ctx context.Context, run *store.Run) []queue.Steer {
	if r.bus == nil {
		return nil
	}
	kept, err := queue.KeepSteers(ctx, r.bus, r.store, run.ThreadID, run.ID, r.finishDeadline(r.steerBudget()))
	if err != nil {
		slog.Error("keeping undelivered steers failed; the sweeper will store them", "run_id", run.ID, "error", err)
	}
	return kept
}

// steerBudget bounds the retries of keeping a run's steers (a third of FinishRetry).
func (r *Runtime) steerBudget() time.Duration { return r.opts.FinishRetry / 3 }

// sweepSteers stores the pending steers of finished runs whose runtime could
// not store them (Redis or Postgres down at finalize).
func (r *Runtime) sweepSteers(ctx context.Context) {
	if r.bus == nil {
		return
	}
	ids, err := r.bus.SteerRuns(ctx)
	if err != nil {
		slog.Warn("sweeper: listing runs with pending steers failed", "error", err)
		return
	}
	for _, id := range ids {
		run, err := r.store.GetRun(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			_ = r.bus.AckSteers(ctx, id, queue.MaxSteers)
			continue
		case err != nil || !store.IsTerminal(run.Status):
			continue
		}
		kept, err := queue.KeepSteers(ctx, r.bus, r.store, run.ThreadID, run.ID, time.Now().Add(time.Second))
		if err != nil {
			slog.Warn("sweeper: storing pending steers failed, will retry", "run_id", id, "error", err)
		} else if len(kept) > 0 {
			slog.Info("sweeper: stored pending steers of a finished run", "run_id", id, "steers", len(kept))
		}
	}
}

// announceSteers tells clients which steers were kept for the next run (before the terminal event).
func announceSteers(ctx context.Context, w eventlog.Writer, steers []queue.Steer) error {
	for _, s := range steers {
		if _, err := w.Append(ctx, agui.SteerEvent(s.ID, s.Text, false)); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) safeRun(ctx context.Context, in agent.Input, w eventlog.Writer) (res agent.Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("agent panic", "run_id", in.Run.ID, "panic", p)
			if r.opts.Reporter != nil {
				r.opts.Reporter.CapturePanic(p)
			}
			err = &agent.Error{Code: agui.CodeInternal, Message: agent.MsgInternal, Detail: fmt.Sprint(p)}
		}
	}()
	return r.exec.Run(ctx, in, w)
}

// outcomeOf maps a terminal event to the run status it implies.
func outcomeOf(e store.Event) (status, code, msg string) {
	var ev struct {
		Outcome struct {
			Type string `json:"type"`
		} `json:"outcome"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(e.Data, &ev)
	switch {
	case e.Type == agui.RunFinished && ev.Outcome.Type == "cancelled":
		return store.StatusCancelled, "", ""
	case e.Type == agui.RunFinished:
		return store.StatusCompleted, "", ""
	case ev.Code == agui.CodeInterrupted:
		return store.StatusInterrupted, ev.Code, ev.Message
	default:
		return store.StatusFailed, ev.Code, ev.Message
	}
}

// errorCode is the RUN_ERROR code of a terminal event ("" for RUN_FINISHED).
func errorCode(final agui.Event) string {
	code, _ := final["code"].(string)
	return code
}

func (r *Runtime) classify(runCtx context.Context, run *store.Run, res agent.Result, err error) (string, string, agui.Event) {
	if cause := context.Cause(runCtx); runCtx.Err() != nil {
		switch {
		case errors.Is(cause, errLost):
			return "", "", nil
		case errors.Is(cause, ErrCancelled):
			return store.StatusCancelled, "", agui.RunFinishedEvent(run.ThreadID, run.ID, "cancelled", res.Usage)
		default:
			return store.StatusInterrupted, InterruptedMessage, agui.RunErrorEvent(agui.CodeInterrupted, InterruptedMessage)
		}
	}
	if err == nil {
		return store.StatusCompleted, "", successEvent(run, res)
	}
	var ae *agent.Error
	if errors.As(err, &ae) {
		slog.Warn("run failed", "run_id", run.ID, "code", ae.Code, "detail", ae.Detail)
		return store.StatusFailed, ae.Message, agui.RunErrorEvent(ae.Code, ae.Message)
	}
	slog.Error("run failed", "run_id", run.ID, "error", err)
	if r.opts.Reporter != nil {
		r.opts.Reporter.CaptureError(err)
	}
	return store.StatusFailed, agent.MsgInternal, agui.RunErrorEvent(agui.CodeInternal, agent.MsgInternal)
}

// heartbeatKey is the Redis liveness signal of a run (value: its runtime id),
// kept for StaleAge: the sweeper spares runs that have one, so a Postgres
// outage longer than StaleAge does not get live runs interrupted.
func heartbeatKey(runID string) string { return "work:run:" + runID + ":hb" }

func (r *Runtime) heartbeat(run *store.Run, cancel context.CancelCauseFunc, done <-chan struct{}) {
	t := time.NewTimer(r.opts.Heartbeat)
	defer t.Stop()
	r.redisHeartbeat(run.ID)
	defer r.clearHeartbeat(run.ID)
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		r.redisHeartbeat(run.ID)
		ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		owned, cancelRequested, err := r.store.Heartbeat(ctx, run.ID, r.opts.ID)
		c()
		next := r.opts.Heartbeat
		switch {
		case err != nil:
			// Retry soon: the first write after Postgres is back refreshes heartbeat_at.
			slog.Warn("heartbeat failed", "run_id", run.ID, "error", err)
			next = min(next, time.Second)
		case !owned:
			cancel(errLost)
		case cancelRequested:
			cancel(ErrCancelled)
		}
		t.Reset(next)
	}
}

func (r *Runtime) redisHeartbeat(runID string) {
	if r.rdb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.rdb.Set(ctx, heartbeatKey(runID), r.opts.ID, r.opts.StaleAge).Err(); err != nil {
		slog.Warn("redis heartbeat failed", "run_id", runID, "error", err)
	}
}

func (r *Runtime) clearHeartbeat(runID string) {
	if r.rdb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = r.rdb.Del(ctx, heartbeatKey(runID)).Err()
}

// aliveInRedis reports a run whose runtime heartbeats in Redis (or whose
// liveness cannot be checked: never interrupt on a Redis error).
func (r *Runtime) aliveInRedis(ctx context.Context) func(*store.Run) bool {
	return func(run *store.Run) bool {
		if r.rdb == nil {
			return false
		}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		n, err := r.rdb.Exists(cctx, heartbeatKey(run.ID)).Result()
		return err != nil || n > 0
	}
}

func (r *Runtime) sweepLoop(ctx context.Context) {
	t := time.NewTicker(r.opts.SweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Sweep(ctx)
		}
	}
}

// Sweep re-enqueues stale queued runs and interrupts runs whose runtime stopped heartbeating.
func (r *Runtime) Sweep(ctx context.Context) {
	if r.rdb != nil {
		ttl := r.opts.SweepEvery - time.Second
		if ttl < time.Second {
			ttl = time.Second
		}
		ok, err := r.rdb.SetNX(ctx, "work:sweeper:lock", r.opts.ID, ttl).Result()
		if err != nil || !ok {
			return
		}
	}
	ids, err := r.store.ListQueuedRunsOlderThan(ctx, r.opts.QueuedAge)
	if err != nil {
		slog.Warn("sweeper: listing queued runs failed", "error", err)
	}
	for _, id := range ids {
		if err := r.q.Enqueue(ctx, id); err != nil {
			slog.Warn("sweeper: re-enqueue failed", "run_id", id, "error", err)
		}
	}
	r.queueHygiene(ctx)
	if n, err := r.log.Repair(ctx); err != nil {
		slog.Warn("sweeper: event repair failed", "error", err)
	} else if n > 0 {
		slog.Info("sweeper: repaired run event logs", "runs", n)
	}
	stale, err := r.store.InterruptStaleRuns(ctx, r.opts.StaleAge, r.aliveInRedis(ctx))
	if err != nil {
		slog.Warn("sweeper: interrupting stale runs failed", "error", err)
	}
	for _, run := range stale {
		slog.Warn("sweeper: run interrupted (runtime stopped heartbeating)", "run_id", run.ID, "runtime_id", run.RuntimeID)
		r.finishInterrupted(ctx, run, r.keepUndeliveredSteers(ctx, run))
	}
	r.sweepSteers(ctx)
	r.purge(ctx)
	r.runJobs(ctx)
}

// queueHygiene reclaims queue entries left pending by stopped runtimes (their
// runs are enqueued again when still queued, the entries acked) and deletes
// the consumers of stopped runtimes.
func (r *Runtime) queueHygiene(ctx context.Context) {
	msgs, err := r.q.Reclaim(ctx, r.opts.ID, r.opts.QueuedAge)
	if err != nil {
		slog.Warn("sweeper: reclaiming pending queue entries failed", "error", err)
	}
	for _, m := range msgs {
		if run, err := r.store.GetRun(ctx, m.RunID); err == nil && run.Status == store.StatusQueued {
			if err := r.q.Enqueue(ctx, m.RunID); err != nil {
				slog.Warn("sweeper: re-enqueue failed", "run_id", m.RunID, "error", err)
				continue
			}
		}
		if err := r.q.Ack(ctx, m.StreamID); err != nil {
			slog.Warn("sweeper: ack of a reclaimed entry failed", "error", err)
		}
	}
	if n, err := r.q.PruneConsumers(ctx, r.opts.ID, r.opts.ConsumerIdle); err != nil {
		slog.Warn("sweeper: pruning queue consumers failed", "error", err)
	} else if n > 0 {
		slog.Info("sweeper: deleted queue consumers of stopped runtimes", "consumers", n)
	}
}

// runJobs runs the periodic jobs that are due (across runtimes when Redis is set).
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
		if r.rdb != nil {
			if ok, err := r.rdb.SetNX(ctx, "work:job:"+j.Name+":lock", r.opts.ID, j.Every).Result(); err != nil || !ok {
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

// purge runs the retention job at most every PurgeEvery (across runtimes when Redis is set).
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
	if r.rdb != nil {
		if ok, err := r.rdb.SetNX(ctx, "work:purge:lock", r.opts.ID, r.opts.PurgeEvery).Result(); err != nil || !ok {
			return
		}
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := r.opts.Purger.Purge(pctx); err != nil {
		slog.Warn("sweeper: retention purge failed", "error", err)
	}
}

// finishInterrupted appends RUN_ERROR after the highest seq written so far. A
// runtime that is still alive may append concurrently: on conflict, re-read and retry.
func (r *Runtime) finishInterrupted(ctx context.Context, run *store.Run, leftover []queue.Steer) {
	for attempt := 0; attempt < 5; attempt++ {
		w, err := r.log.Resume(ctx, run.ID, run.LastSeq)
		if err != nil {
			slog.Error("sweeper: resume event log failed", "run_id", run.ID, "error", err)
			return
		}
		if end, ok := w.Terminal(); ok {
			// The runtime did finish (e.g. Postgres was down at run end): never add a
			// second terminal event; make the row match the log instead.
			status, code, msg := outcomeOf(end)
			if err := r.store.SetRunOutcome(ctx, run.ID, status, code, msg); err != nil {
				slog.Error("sweeper: correcting run status failed", "run_id", run.ID, "error", err)
			}
			_ = w.Close(ctx)
			return
		}
		if w.LastSeq() == 0 {
			// The runtime died before its first event: strict AG-UI order.
			_, err = w.Append(ctx, agui.RunStartedEvent(run.ThreadID, run.ID))
		}
		if err == nil {
			err = announceSteers(ctx, w, leftover)
		}
		if err == nil {
			_, err = w.Append(ctx, agui.RunErrorEvent(agui.CodeInterrupted, InterruptedMessage))
		}
		if errors.Is(err, eventlog.ErrConflict) {
			continue
		}
		if err != nil {
			slog.Error("sweeper: writing RUN_ERROR failed", "run_id", run.ID, "error", err)
		}
		_ = w.Close(ctx)
		return
	}
	slog.Error("sweeper: gave up writing RUN_ERROR (log keeps moving)", "run_id", run.ID)
}

// ErrorReporter receives what the runtime logs as internal failures.
type ErrorReporter interface {
	CaptureError(err error)
	CapturePanic(recovered any)
}
