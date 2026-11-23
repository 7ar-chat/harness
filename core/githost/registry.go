package githost

import (
	"context"
	"time"
)

// Entry is a registered host as stored: the host and, when an administrator set one, the sealed shared service
// token that serves users who saved none of their own (the host side seals it; the store never reads it).
type Entry struct {
	Host        Host      `json:"host"`
	SealedToken []byte    `json:"sealedToken,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// Registry persists the hosts an administrator registered. Hosts are keyed by Host.Name.
type Registry interface {
	// List returns every entry, ordered by name.
	List(ctx context.Context) ([]Entry, error)
	// Get returns the entry, or ErrUnknownHost.
	Get(ctx context.Context, name string) (Entry, error)
	// Put creates or replaces the entry.
	Put(ctx context.Context, e Entry) error
	// Delete removes the entry (no error when absent).
	Delete(ctx context.Context, name string) error
}
