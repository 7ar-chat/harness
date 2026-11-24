package harness

import (
	"context"
	"log/slog"
	"sync"

	"github.com/7ar-chat/harness/core/store"
)

// threadHooks are what the host asked to be told when a conversation is deleted.
type threadHooks struct {
	mu  sync.Mutex
	fns []func(ctx context.Context, threadID string)
}

// OnThreadDeleted registers fn, called after a thread is deleted through DeleteThread, with the id of the thread: the
// host drops what it keeps about conversations (a schedule's reused session, notifications).
func (h *Harness) OnThreadDeleted(fn func(ctx context.Context, threadID string)) {
	h.threadHooks.mu.Lock()
	h.threadHooks.fns = append(h.threadHooks.fns, fn)
	h.threadHooks.mu.Unlock()
}

// DeleteThread deletes a thread of user and everything the harness keeps beside its messages: the documents of the
// thread-state store (the queue of waiting messages, the chosen environment, the published artifacts index, ...) and
// whatever the host registered with OnThreadDeleted. It returns the deleted thread, store.ErrNotFound when user has
// no such thread (nothing is cleaned then).
func (h *Harness) DeleteThread(ctx context.Context, user, id string) (*store.Thread, error) {
	th, err := h.Store.DeleteThread(ctx, user, id)
	if err != nil {
		return th, err
	}
	h.ThreadDeleted(ctx, id)
	return th, nil
}

// ThreadDeleted drops what is kept beside the messages of a thread that is already gone from the Store (for a host
// that deleted it by another path). Failures are logged: the thread is gone either way.
func (h *Harness) ThreadDeleted(ctx context.Context, id string) {
	ctx = context.WithoutCancel(ctx)
	if h.ThreadState != nil {
		if err := h.ThreadState.DeleteThreadState(ctx, id); err != nil {
			slog.Warn("harness: dropping the state of a deleted thread failed", "thread_id", id, "error", err)
		}
	}
	h.threadHooks.mu.Lock()
	fns := append([]func(context.Context, string){}, h.threadHooks.fns...)
	h.threadHooks.mu.Unlock()
	for _, fn := range fns {
		fn(ctx, id)
	}
}
