package store

import (
	"context"
	"encoding/json"
)

// ThreadStateStore keeps small JSON documents per thread and key: what a feature remembers about a conversation
// beside its messages (the queue of messages waiting for the running reply, the runtime environment chosen for
// the thread, the artifacts published into answers). Mem, File and PG implementations live in
// adapters/store/threadstate; every replica of a deployment sees the same documents.
type ThreadStateStore interface {
	// GetThreadState returns the document of key, nil when there is none.
	GetThreadState(ctx context.Context, threadID, key string) (json.RawMessage, error)
	// UpdateThreadState reads the document of key (nil when there is none), calls fn and stores what it
	// returns; a nil result deletes the document. Updates of one (thread, key) are serialised across
	// processes: fn sees the document the previous update wrote. An error from fn leaves the document as it was.
	UpdateThreadState(ctx context.Context, threadID, key string, fn func(old json.RawMessage) (json.RawMessage, error)) error
	// DeleteThreadState drops every document of a thread (the thread is gone).
	DeleteThreadState(ctx context.Context, threadID string) error
}
