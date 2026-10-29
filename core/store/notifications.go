package store

import (
	"context"
	"time"
)

// LastRun is the latest run of a thread (harness v3 §8), FinishedAt nil while active.
type LastRun struct {
	ID         string
	Status     string
	FinishedAt *time.Time
}

// NotificationStore tracks which threads finished work while their owner was away.
type NotificationStore interface {
	// MarkThreadSeen sets the thread's last_seen_at to now (ErrNotFound).
	MarkThreadSeen(ctx context.Context, login, id string) error
	// CountUnreadThreads counts the caller's unread threads.
	CountUnreadThreads(ctx context.Context, login string) (int, error)
	// SearchUnreadThreads is SearchThreads restricted to unread threads.
	SearchUnreadThreads(ctx context.Context, login, search string, limit int, before *ThreadCursor) ([]*Thread, error)
}
