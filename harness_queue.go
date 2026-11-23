package harness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/adapters/store/threadstate"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/queue"
	"github.com/7ar-chat/harness/engine/runtime"
	"github.com/7ar-chat/harness/features/artifacts"
	"github.com/7ar-chat/harness/features/chatqueue"
)

// QueuedMessage is a message waiting for the running reply of its thread.
type QueuedMessage = chatqueue.Item

// PublishedArtifact is a workspace file the agent published into an answer.
type PublishedArtifact = artifacts.Published

// Steer errors, as the HTTP layer reports them (the engine's own values).
var (
	ErrSteersClosed  = queue.ErrSteersClosed
	ErrTooManySteers = queue.ErrTooManySteers
)

// PublishedArtifacts lists the artifacts the agent published on the thread, oldest first.
func (h *Harness) PublishedArtifacts(ctx context.Context, user, threadID string) ([]PublishedArtifact, error) {
	if err := h.ownsThread(ctx, user, threadID); err != nil {
		return nil, err
	}
	return artifacts.ListPublished(ctx, h.ThreadState, threadID)
}

// defaultThreadState picks the thread-state store that lives where the Store does: the Postgres table beside a
// pgstore, a JSON file in the directory of a local store, memory otherwise.
func defaultThreadState(st Store) (store.ThreadStateStore, error) {
	switch s := st.(type) {
	case interface{ Pool() *pgxpool.Pool }:
		return threadstate.NewPG(s.Pool()), nil
	case interface{ Dir() string }:
		if dir := s.Dir(); dir != "" {
			return threadstate.OpenFile(filepath.Join(dir, "thread_state.json"))
		}
	}
	return threadstate.NewMem(), nil
}

// QueueMessage queues a message for a thread whose reply is still running: it is sent as a run of its own, with
// o, when the running one completes. Without a running reply it is sent at once (the returned run is then set
// and the item is already gone from the queue). Attachments are not queued.
func (h *Harness) QueueMessage(ctx context.Context, user, threadID, text string, o RunOptions) (chatqueue.Item, *store.Run, error) {
	if err := h.ownsThread(ctx, user, threadID); err != nil {
		return chatqueue.Item{}, nil, err
	}
	if len(o.Attachments) > 0 {
		return chatqueue.Item{}, nil, fmt.Errorf("%w: attachments cannot be queued", ErrInvalidRequest)
	}
	if o.Model == "" {
		return chatqueue.Item{}, nil, fmt.Errorf("%w: model is required", ErrInvalidRequest)
	}
	if !h.cfg.ModelAllowed(o.Model) {
		return chatqueue.Item{}, nil, fmt.Errorf("%w: model %q is not allowed", ErrInvalidRequest, o.Model)
	}
	if _, err := h.sandboxMode(o.Sandbox); err != nil {
		return chatqueue.Item{}, nil, err
	}
	if _, err := runMode(o.Mode); err != nil {
		return chatqueue.Item{}, nil, err
	}
	it, err := h.Queued.Add(ctx, threadID, text, chatqueue.Options{
		Model: o.Model, ReasoningEffort: o.ReasoningEffort, Skills: o.Skills, Interactive: o.Interactive,
		Mode: string(o.Mode), Sandbox: string(o.Sandbox), Role: o.Role,
		Author: o.Author, AuthorKind: o.AuthorKind, OnBehalfOf: o.OnBehalfOf,
	})
	if errors.Is(err, chatqueue.ErrEmpty) || errors.Is(err, chatqueue.ErrFull) {
		return it, nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if err != nil {
		return it, nil, err
	}
	// The reply may have ended between the caller's look and the add: nobody would be left to send the message.
	run, err := h.DrainQueue(ctx, user, threadID)
	return it, run, err
}

// ListQueue returns the messages waiting on the thread, in sending order.
func (h *Harness) ListQueue(ctx context.Context, user, threadID string) ([]chatqueue.Item, error) {
	if err := h.ownsThread(ctx, user, threadID); err != nil {
		return nil, err
	}
	return h.Queued.List(ctx, threadID)
}

// RemoveQueued drops a waiting message (store.ErrNotFound when it is not waiting any more).
func (h *Harness) RemoveQueued(ctx context.Context, user, threadID, id string) error {
	if err := h.ownsThread(ctx, user, threadID); err != nil {
		return err
	}
	_, ok, err := h.Queued.Remove(ctx, threadID, id)
	if err == nil && !ok {
		err = store.ErrNotFound
	}
	return err
}

// SendQueuedNow takes a waiting message out of the queue and delivers it at once: as a steer of the running
// reply (steered=true), or as a run when the thread is idle. store.ErrNotFound when it is not waiting any more.
func (h *Harness) SendQueuedNow(ctx context.Context, user, threadID, id string) (steered bool, run *store.Run, err error) {
	th, err := h.Store.GetThread(ctx, user, threadID)
	if err != nil {
		return false, nil, err
	}
	if th.ActiveRun != nil {
		it, ok, err := h.Queued.Remove(ctx, threadID, id)
		if err != nil {
			return false, nil, err
		}
		if !ok {
			return false, nil, store.ErrNotFound
		}
		err = h.SteerAs(ctx, user, th.ActiveRun.ID, it.Text, Author{Login: it.Options.Author, Kind: it.Options.AuthorKind, OnBehalfOf: it.Options.OnBehalfOf})
		if err != nil {
			// The reply ended meanwhile (or took no more steers): the message goes back to the front, and the
			// hook of that run, or the caller's next try, sends it.
			if perr := h.Queued.Prepend(ctx, threadID, it); perr != nil {
				slog.Warn("queue: putting a message back failed", "thread_id", threadID, "error", perr)
			}
			if errors.Is(err, queue.ErrSteersClosed) {
				run, derr := h.DrainQueue(ctx, user, threadID)
				return false, run, derr
			}
			return false, nil, err
		}
		return true, nil, nil
	}
	items, err := h.Queued.List(ctx, threadID)
	if err != nil {
		return false, nil, err
	}
	for _, it := range items {
		if it.ID == id {
			run, err := h.sendQueued(ctx, user, threadID, it)
			return false, run, err
		}
	}
	return false, nil, store.ErrNotFound
}

// DrainQueue sends the first waiting message as a run when the thread has no active run. It is idempotent
// and safe to call from any replica: the message is sent under its own id, so a second sender is refused by the
// store (the thread is busy, or the id exists), and removing it from the queue is idempotent.
func (h *Harness) DrainQueue(ctx context.Context, user, threadID string) (*store.Run, error) {
	it, ok, err := h.Queued.Head(ctx, threadID)
	if err != nil || !ok {
		return nil, err
	}
	return h.sendQueued(ctx, user, threadID, it)
}

func (h *Harness) sendQueued(ctx context.Context, user, threadID string, it chatqueue.Item) (*store.Run, error) {
	o := it.Options
	run, err := h.Send(ctx, user, threadID, it.Text, RunOptions{
		Model: o.Model, ReasoningEffort: o.ReasoningEffort, Skills: o.Skills, Interactive: o.Interactive,
		Sandbox: SandboxMode(o.Sandbox), Mode: RunMode(o.Mode), Role: o.Role, MessageID: it.ID,
		Author: o.Author, AuthorKind: o.AuthorKind, OnBehalfOf: o.OnBehalfOf,
	})
	switch {
	case err == nil:
	case errors.Is(err, store.ErrRunActive):
		return nil, nil // busy: the run that ends next sends it
	case errors.Is(err, store.ErrConflict):
		err = nil // another replica sent it already
	case errors.Is(err, store.ErrNotFound):
		return nil, err // the thread is gone
	default:
		// A message that cannot be sent (a model no longer allowed...) must not block the ones behind it.
		slog.Warn("queue: dropping a message that cannot be sent", "thread_id", threadID, "error", err)
		err = nil
	}
	if _, _, rerr := h.Queued.Remove(ctx, threadID, it.ID); rerr != nil {
		slog.Warn("queue: removing a sent message failed", "thread_id", threadID, "error", rerr)
	}
	return run, err
}

// queueHook sends the next waiting message when a reply completes (runtime.Hook). It runs in the process that
// executed the run, after the run's status is recorded, so the thread is idle and any replica doing the same
// at the same moment is refused by the store.
type queueHook struct{ h *Harness }

func (q queueHook) Finishing(_ context.Context, f *runtime.Finish) func(context.Context) {
	run := f.Run
	if f.Status != store.StatusCompleted || f.Result.Interrupt != nil || (run.Kind != "" && run.Kind != store.KindChat) {
		return nil // a failed, cancelled or waiting run leaves the queue for the user to decide
	}
	return func(ctx context.Context) {
		if _, err := q.h.DrainQueue(ctx, run.UserLogin, run.ThreadID); err != nil {
			slog.Warn("queue: sending the next message failed", "thread_id", run.ThreadID, "error", err)
		}
	}
}
