package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/llm"
)

// Budgets of a Caller whose fields are not set.
const (
	DefaultFirstChunk = 300 * time.Second
	DefaultIdle       = 90 * time.Second
	DefaultCeiling    = 20 * time.Minute
)

// ErrStalled is the cause of a call cut by the watchdog.
var ErrStalled = errors.New("the model stream was silent for too long")

// CallError is a model call that failed and will not be retried (or ran out of retries). Class says how
// the failure was sorted; Err is the provider's error.
type CallError struct {
	Err   error
	Class llm.ErrorClass
}

func (e *CallError) Error() string { return e.Err.Error() }
func (e *CallError) Unwrap() error { return e.Err }

// Attempt is the caller's view of one streamed call. A new one is made for every attempt that got as far as a
// stream, so a retried call starts from a clean state.
type Attempt interface {
	// Chunk receives each chunk in order. An error ends the whole call at once and is returned unchanged.
	Chunk(ctx context.Context, chunk *ai.Message) error
	// Settle is called when the stream is over: complete, or broken because ctx was cancelled mid-stream. An
	// error ends the call like one from Chunk.
	Settle(ctx context.Context, broken bool) error
	// Abort is called when the attempt failed; retry says another attempt follows. An error ends the call.
	Abort(ctx context.Context, retry bool) error
}

// Stream is the chunks of a call that completed, or that was cancelled part-way (then Call also returns ctx.Err()).
type Stream struct {
	Chunks []*ai.Message
	// Refusal is the refusal text the provider streamed, if any (ai/llm CallInfo).
	Refusal string
}

// Caller makes one model call as a stream under a watchdog and re-issues it when it fails in a way worth
// retrying. The watchdog cancels an attempt whose first chunk takes longer than FirstChunk, whose chunks come
// further apart than Idle, or that lasts longer than Ceiling; an event the provider sends that is not a chunk
// restarts the current budget (a model reasoning silently is not stalled), a keep-alive comment does not.
type Caller struct {
	// FirstChunk, Idle and Ceiling are the budgets (0: the Default values).
	FirstChunk, Idle, Ceiling time.Duration
	// Retries is how often a failed call is re-issued (0: never).
	Retries int
	// Backoff is the first retry delay (llm.Backoff doubles it, with jitter, and never goes below Retry-After).
	Backoff time.Duration
	// Sleep waits between retries (nil: a timer).
	Sleep func(ctx context.Context, d time.Duration) error
	// OnRetry is called after a failed attempt was aborted and before the wait, with the number of the attempt
	// that failed. An error ends the call.
	OnRetry func(ctx context.Context, attempt int, class llm.ErrorClass, err error) error
}

type attemptResult struct {
	out *Stream
	at  Attempt
	err error
	// class sorts err (timeout when the watchdog cut the stream).
	class      llm.ErrorClass
	retryAfter time.Duration
	// fatal is an error of the caller's own hooks: the call ends without retry.
	fatal error
}

// Call streams m's answer to msgs. A failure that is retryable and within Retries is aborted (Attempt.Abort) and
// re-issued; any other is returned as *CallError. A cancelled ctx returns ctx.Err() with the part of the stream
// received (nil when none). newAttempt is called once per attempt that obtained a stream.
func (c *Caller) Call(ctx context.Context, m ai.ChatModel, msgs []*ai.Message, opts []ai.Option, newAttempt func() Attempt) (*Stream, error) {
	for attempt := 1; ; attempt++ {
		res := c.attempt(ctx, m, msgs, opts, newAttempt)
		switch {
		case res.fatal != nil:
			return nil, res.fatal
		case res.err == nil:
			return res.out, nil
		case ctx.Err() != nil:
			return res.out, ctx.Err()
		}
		retry := res.class.Retryable() && attempt <= c.Retries
		if res.at != nil {
			if err := res.at.Abort(ctx, retry); err != nil {
				return nil, err
			}
		}
		if !retry {
			return nil, &CallError{Err: res.err, Class: res.class}
		}
		if c.OnRetry != nil {
			if err := c.OnRetry(ctx, attempt, res.class, res.err); err != nil {
				return nil, err
			}
		}
		if err := c.sleep(ctx, llm.Backoff(attempt, c.Backoff, res.retryAfter, rand.Float64())); err != nil {
			return nil, err
		}
	}
}

func (c *Caller) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// attempt is one streamed call.
func (c *Caller) attempt(ctx context.Context, m ai.ChatModel, msgs []*ai.Message, opts []ai.Option, newAttempt func() Attempt) attemptResult {
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	actx, info := llm.WithCallInfo(actx)
	wd := startWatchdog(cancel, or(c.FirstChunk, DefaultFirstChunk), or(c.Idle, DefaultIdle), or(c.Ceiling, DefaultCeiling))
	defer wd.stop()
	info.OnActivity(wd.activity)
	fail := func(at Attempt, err error) attemptResult {
		class := llm.Classify(err)
		if wd.fired.Load() && ctx.Err() == nil {
			class, err = llm.ClassTimeout, fmt.Errorf("%w: %v", ErrStalled, err)
		}
		return attemptResult{at: at, err: err, class: class, retryAfter: info.RetryAfter()}
	}

	sr, err := m.Stream(actx, msgs, opts...)
	if err != nil {
		return fail(nil, err)
	}
	defer sr.Close()
	at := newAttempt()
	var chunks []*ai.Message
	var streamErr error
	for {
		chunk, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			streamErr = err
			break
		}
		wd.kick()
		chunks = append(chunks, chunk)
		if err := at.Chunk(ctx, chunk); err != nil {
			return attemptResult{fatal: err}
		}
	}
	wd.stop()
	if streamErr != nil && ctx.Err() == nil {
		return fail(at, streamErr)
	}
	if err := at.Settle(ctx, streamErr != nil); err != nil {
		return attemptResult{fatal: err}
	}
	out := &Stream{Chunks: chunks, Refusal: info.Refusal()}
	if streamErr != nil { // cancelled: the partial answer is kept
		return attemptResult{out: out, at: at, err: ctx.Err()}
	}
	return attemptResult{out: out, at: at}
}

// watchdog cancels an attempt that is silent for longer than its budget.
type watchdog struct {
	mu      sync.Mutex
	timer   *time.Timer
	ceiling *time.Timer
	first   time.Duration
	idle    time.Duration
	started bool
	fired   atomic.Bool
}

func startWatchdog(cancel context.CancelFunc, first, idle, ceiling time.Duration) *watchdog {
	w := &watchdog{first: first, idle: idle}
	fire := func() {
		w.fired.Store(true)
		cancel()
	}
	w.timer = time.AfterFunc(first, fire)
	w.ceiling = time.AfterFunc(ceiling, fire) // whatever the stream sends
	return w
}

// kick: a chunk arrived; the next one must follow within idle.
func (w *watchdog) kick() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.started = true
	if !w.fired.Load() {
		w.timer.Reset(w.idle)
	}
}

// activity: the stream is alive; its current budget starts again.
func (w *watchdog) activity() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fired.Load() {
		return
	}
	if w.started {
		w.timer.Reset(w.idle)
	} else {
		w.timer.Reset(w.first)
	}
}

func (w *watchdog) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.timer.Stop()
	w.ceiling.Stop()
}
