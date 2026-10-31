// Package runtime claims queued runs, drives the agent and finishes runs.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/eventlog"
	"github.com/7ar-chat/harness/engine/queue"
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

// Locks is the small shared state the runtime needs to cooperate with other
// runtimes: the sweeper and job locks, and the liveness marks of runs. The
// Redis implementation is redisx.Coordinator; nil means a single process.
type Locks interface {
	// SetNX stores key=value for ttl unless the key exists.
	SetNX(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	// Set stores key=value for ttl.
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	// Del removes key.
	Del(ctx context.Context, key string) (bool, error)
	// Exists reports whether key is set.
	Exists(ctx context.Context, key string) (bool, error)
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
	ReadBlock     time.Duration // default 2 s (bounds shutdown latency: a blocking Redis read ignores ctx cancel)
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

// Runtime is a run worker.
type Runtime struct {
	opts  Options
	store store.Full
	log   eventlog.Log
	q     RunQueue
	bus   queue.Bus
	exec  Executor
	locks Locks

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

// New builds a runtime. locks is used for the sweeper lock and run liveness (nil: single process).
func New(opts Options, st store.Full, log eventlog.Log, q RunQueue, bus queue.Bus, exec Executor, locks Locks) *Runtime {
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
	return &Runtime{opts: opts, store: st, log: log, q: q, bus: bus, exec: exec, locks: locks,
		inflight: map[string]context.CancelCauseFunc{}, sem: make(chan struct{}, opts.Concurrency)}
}

// ErrorReporter receives what the runtime logs as internal failures.
type ErrorReporter interface {
	CaptureError(err error)
	CapturePanic(recovered any)
}
