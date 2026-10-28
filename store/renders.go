package store

import (
	"context"
	"time"
)

// Render is a view_file render of a thread's document (harness v3 "Review
// round 1"): its page images are /workspace/.work/renders/{RenderID}/p{N}.png
// and they are only served when the source's classification allows it.
type Render struct {
	ThreadID   string
	RenderID   string
	SourcePath string
	// Label is the most restrictive confidentiality label of the source ("" when none).
	Label string
	// Allowed reports a source whose label allows derived files (classification.SourceAllowed).
	Allowed   bool
	Pages     []int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// RenderStore persists render metadata.
type RenderStore interface {
	// PutRender records a render; the pages of an earlier record of the same
	// render are kept (union, sorted) and its source and label replaced.
	PutRender(ctx context.Context, r *Render) error
	// GetRender returns a render of a thread (ErrNotFound).
	GetRender(ctx context.Context, threadID, renderID string) (*Render, error)
}
