package store

import (
	"context"
	"time"
)

// RunCredential is the encrypted access token of a run's user (web search).
// Sealed is opaque to the store (AES-GCM, see internal/websearch).
type RunCredential struct {
	RunID     string
	UserLogin string
	Sealed    []byte
	ExpiresAt time.Time
}

// CredentialStore persists run credentials (table work_run_credentials).
type CredentialStore interface {
	// PutRunCredential stores c when its run is queued or running and belongs to
	// c.UserLogin. An existing credential is replaced when replace is true or c
	// expires later. stored reports whether a row was written.
	PutRunCredential(ctx context.Context, c RunCredential, replace bool) (stored bool, err error)
	// GetRunCredential returns ErrNotFound when the run has none.
	GetRunCredential(ctx context.Context, runID string) (*RunCredential, error)
	DeleteRunCredential(ctx context.Context, runID string) error
	// PurgeRunCredentials deletes the credentials of terminal runs and the expired ones.
	PurgeRunCredentials(ctx context.Context) (int64, error)
}
