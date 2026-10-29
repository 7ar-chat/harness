// Package queue holds the Redis-backed run queue, the per-run control channel
// (cancel) and the per-run steer list.
package queue

import (
	"context"
	"errors"
	"time"

	"github.com/GhaziBenDahmane/harness/core/store"
)

const (
	// Stream is the run queue.
	Stream = "work:runs"
	// Group is the consumer group of runtimes.
	Group = "runtime"
	// MaxSteers is the cap of pending steer messages per run.
	MaxSteers = 20
)

var (
	// ErrTooManySteers is returned when MaxSteers are already pending.
	ErrTooManySteers = errors.New("too many pending steer messages")
	// ErrSteersClosed is returned once the run stopped accepting steers (final step or finished).
	ErrSteersClosed = errors.New("the run no longer accepts messages")
)

// Steer is a user message injected into a running run.
type Steer struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	// Author is who sent the steer, with AuthorKind and OnBehalfOf as on store.Message.
	Author     string `json:"author,omitempty"`
	AuthorKind string `json:"authorKind,omitempty"`
	OnBehalfOf string `json:"onBehalfOf,omitempty"`
}

// Message is a queue entry.
type Message struct {
	StreamID string
	RunID    string
}

// Queue enqueues runs.
type Queue interface {
	Enqueue(ctx context.Context, runID string) error
}

// Bus carries per-run control signals and steers.
type Bus interface {
	PublishCancel(ctx context.Context, runID string) error
	// SubscribeCancel calls fn when a cancel is published; the returned func unsubscribes.
	SubscribeCancel(ctx context.Context, runID string, fn func()) (func(), error)
	// PushSteer fails with ErrSteersClosed after FinalizeSteers.
	PushSteer(ctx context.Context, runID string, s Steer) error
	DrainSteers(ctx context.Context, runID string) ([]Steer, error)
	// FinalizeSteers atomically stops accepting steers and returns the pending ones.
	FinalizeSteers(ctx context.Context, runID string) ([]Steer, error)
	// ReopenSteers accepts steers again (the run continues after all).
	ReopenSteers(ctx context.Context, runID string) error
	// CloseSteers stops accepting steers and returns the pending ones without
	// removing them: AckSteers removes them once they are stored elsewhere.
	CloseSteers(ctx context.Context, runID string) ([]Steer, error)
	// AckSteers removes the first n pending steers (stored by the caller).
	AckSteers(ctx context.Context, runID string, n int) error
	// SteerRuns lists the runs that may still hold pending steers (the
	// sweeper stores those of finished runs).
	SteerRuns(ctx context.Context) ([]string, error)
}

// MessageAppender stores thread messages (store.Store).
type MessageAppender interface {
	AppendMessages(ctx context.Context, msgs ...*store.Message) error
}

// KeepSteers closes a run's steer list and stores its pending steers in the
// thread (delivered=false), retrying transient Redis/Postgres errors until
// deadline. It returns the steers that are stored (only those may be announced
// as kept); on error the steers stay pending in Redis, listed by SteerRuns, so
// the sweeper stores them later: an accepted steer is never lost.
func KeepSteers(ctx context.Context, bus Bus, st MessageAppender, threadID, runID string, deadline time.Time) ([]Steer, error) {
	var left []Steer
	if err := retry(ctx, deadline, func(ctx context.Context) error {
		var err error
		left, err = bus.CloseSteers(ctx, runID)
		return err
	}); err != nil {
		return nil, err
	}
	if len(left) == 0 {
		_ = retry(ctx, deadline, func(ctx context.Context) error { return bus.AckSteers(ctx, runID, 0) })
		return nil, nil
	}
	msgs := SteerMessages(threadID, runID, left)
	if err := retry(ctx, deadline, func(ctx context.Context) error { return appendOnce(ctx, st, msgs) }); err != nil {
		return nil, err
	}
	// Stored: a failed ack only makes the sweeper find them stored already.
	_ = retry(ctx, deadline, func(ctx context.Context) error { return bus.AckSteers(ctx, runID, len(left)) })
	return left, nil
}

// appendOnce stores steer messages; ones already stored (same id) count as stored.
func appendOnce(ctx context.Context, st MessageAppender, msgs []*store.Message) error {
	err := st.AppendMessages(ctx, msgs...)
	if !errors.Is(err, store.ErrConflict) {
		return err
	}
	for _, m := range msgs {
		if err := st.AppendMessages(ctx, m); err != nil && !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	return nil
}

// retry runs fn (5 s per attempt) with backoff until it succeeds or deadline passes.
func retry(ctx context.Context, deadline time.Time, fn func(ctx context.Context) error) error {
	for backoff := 50 * time.Millisecond; ; backoff = min(backoff*2, 2*time.Second) {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err := fn(actx)
		cancel()
		if err == nil || time.Now().Add(backoff).After(deadline) {
			return err
		}
		time.Sleep(backoff)
	}
}

// SteerMessages turns steers that will not reach the model of this run into
// thread messages, so the next run sees them.
func SteerMessages(threadID, runID string, steers []Steer) []*store.Message {
	out := make([]*store.Message, 0, len(steers))
	for _, st := range steers {
		out = append(out, &store.Message{ID: st.ID, ThreadID: threadID, RunID: runID, Role: store.RoleUser,
			Content: st.Text, Metadata: map[string]any{"steer": true, "delivered": false},
			AuthorLogin: st.Author, AuthorKind: st.AuthorKind, OnBehalfOf: st.OnBehalfOf})
	}
	return out
}
