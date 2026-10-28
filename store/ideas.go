package store

import (
	"context"
	"time"
)

// Idea usage actions (Work's RequestIdeaUsage.Action).
const (
	IdeaSelected  = "selected"
	IdeaSubmitted = "submitted"
)

// IdeaUsage records that a user selected an idea or submitted its prompt.
type IdeaUsage struct {
	ID        string
	UserLogin string
	// ThreadID is the conversation the usage belongs to ("" when none).
	ThreadID  string
	IdeaID    string
	Action    string
	Surface   string
	Metadata  map[string]any
	CreatedAt time.Time
}

// IdeaStore persists idea usage (table work_idea_usage).
type IdeaStore interface {
	// RecordIdeaUsage stores one usage; an unknown or foreign ThreadID is stored as none.
	RecordIdeaUsage(ctx context.Context, u IdeaUsage) (*IdeaUsage, error)
	// ListIdeaUsage returns a user's usage, newest first.
	ListIdeaUsage(ctx context.Context, login string) ([]*IdeaUsage, error)
}
