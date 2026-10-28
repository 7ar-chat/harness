package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Memory entry statuses: pending entries were written by the model after it
// saw untrusted content and are not used until the user confirms them; deleted
// entries were soft-deleted (purged after the retention by the consolidation job).
const (
	MemoryActive  = "active"
	MemoryPending = "pending"
	MemoryDeleted = "deleted"
)

// Memory entry sources: written by the agent's memory tools, by the user in
// Settings > Memory, or migrated from the former per-user facts.
const (
	MemorySourceProxy  = "proxy"
	MemorySourceUser   = "user"
	MemorySourceLegacy = "legacy"
)

// MemoryLimits caps a user's entries per status (≤ 0: no limit): active ones,
// and pending ones (the agent's proposals after untrusted content, capped apart
// so they cannot crowd out the confirmed memory).
type MemoryLimits struct {
	Active  int
	Pending int
}

func (l MemoryLimits) limitOf(status string) int {
	switch status {
	case MemoryActive:
		return l.Active
	case MemoryPending:
		return l.Pending
	}
	return 0
}

// Of returns the limit of status (0: none).
func (l MemoryLimits) Of(status string) int { return l.limitOf(status) }

var (
	// ErrMemoryLimit means the user already has the maximum number of entries of that status.
	ErrMemoryLimit = errors.New("memory limit reached")
	// ErrMemoryPathExists means another non-deleted entry of the user has this path.
	ErrMemoryPathExists = errors.New("memory path exists")
	// ErrMemoryConflict means the entry changed since it was read (its version differs).
	ErrMemoryConflict = errors.New("memory entry changed")
)

// MemoryEntry is one durable memory of a user (Work's memory catalog entry
// plus its content): a path such as memories/semantic/facts/….md, a title,
// markdown content, tags, an optional importance and free metadata (trigger…).
type MemoryEntry struct {
	ID         string
	UserLogin  string
	Path       string
	Title      string
	Content    string
	Tags       []string
	Importance *float64
	Metadata   map[string]any
	Scope      string
	Repository string
	Source     string
	Status     string
	// SourceThreadID is the conversation that wrote the entry ("" for the user or migrated rows).
	SourceThreadID   string
	ContentHash      string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	ContentUpdatedAt time.Time
	DeletedAt        *time.Time
	// Version is bumped by every update (optimistic concurrency, see UpdateMemoryEntry).
	Version int64
	// Proposal is a change the agent made to this active entry after untrusted
	// content: the entry keeps (and is used with) its confirmed fields until the
	// user keeps the proposal (applied) or discards it (dropped).
	Proposal *MemoryProposal
}

// MemoryProposal is a proposed state of an entry's editable fields.
type MemoryProposal struct {
	Path        string         `json:"path"`
	Title       string         `json:"title"`
	Content     string         `json:"content"`
	Tags        []string       `json:"tags"`
	Importance  *float64       `json:"importance"`
	Metadata    map[string]any `json:"metadata"`
	ContentHash string         `json:"content_hash"`
	// ThreadID is the conversation that proposed it.
	ThreadID   string    `json:"thread_id,omitempty"`
	ProposedAt time.Time `json:"proposed_at"`
}

// Clone returns a deep copy of p.
func (p *MemoryProposal) Clone() *MemoryProposal {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Tags = append([]string{}, p.Tags...)
	if p.Importance != nil {
		v := *p.Importance
		cp.Importance = &v
	}
	cp.Metadata = cloneMap(p.Metadata)
	return &cp
}

// Clone returns a deep copy of e.
func (e *MemoryEntry) Clone() *MemoryEntry {
	if e == nil {
		return nil
	}
	cp := *e
	cp.Tags = append([]string{}, e.Tags...)
	if e.Importance != nil {
		v := *e.Importance
		cp.Importance = &v
	}
	cp.Metadata = cloneMap(e.Metadata)
	if e.DeletedAt != nil {
		t := *e.DeletedAt
		cp.DeletedAt = &t
	}
	cp.Proposal = e.Proposal.Clone()
	return &cp
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneMap(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = cloneValue(item)
		}
		return out
	default:
		return v
	}
}

// LegacyMemoryID maps the uuid of a former work_memories row to the id of the
// entry migration 0010 created for it (so links from old conversations work).
func LegacyMemoryID(id string) string {
	if u, err := uuid.Parse(id); err == nil {
		return "mem_" + strings.ReplaceAll(u.String(), "-", "")
	}
	return id
}

// MemoryStore is the persistence of the memory entries (per user).
type MemoryStore interface {
	// MemoryEnabled is the user's memory setting (default on).
	MemoryEnabled(ctx context.Context, login string) (bool, error)
	SetMemoryEnabled(ctx context.Context, login string, enabled bool) error

	// ListMemoryEntries returns the user's entries with one of statuses (all when
	// empty), oldest first.
	ListMemoryEntries(ctx context.Context, login string, statuses ...string) ([]*MemoryEntry, error)
	// GetMemoryEntry returns one of the user's entries by id (ErrNotFound otherwise).
	GetMemoryEntry(ctx context.Context, login, id string) (*MemoryEntry, error)
	// GetMemoryEntryByPath returns the user's non-deleted entry at path, else the
	// latest deleted one (ErrNotFound when none).
	GetMemoryEntryByPath(ctx context.Context, login, path string) (*MemoryEntry, error)
	// CreateMemoryEntry inserts e; ErrMemoryLimit when the user already has
	// limits.Of(e.Status) entries of that status, ErrMemoryPathExists on a path clash.
	CreateMemoryEntry(ctx context.Context, e *MemoryEntry, limits MemoryLimits) (*MemoryEntry, error)
	// UpdateMemoryEntry stores the mutable fields of e (path, title, content,
	// tags, importance, metadata, status, hashes, timestamps, proposal) for e.UserLogin/e.ID
	// when the stored version is still e.Version (ErrMemoryConflict otherwise:
	// re-read and retry, or give up), and bumps the version. When e moves to
	// another status, the limit of the new one is checked under the same per-user
	// lock as CreateMemoryEntry (ErrMemoryLimit): confirm, restore.
	UpdateMemoryEntry(ctx context.Context, e *MemoryEntry, limits MemoryLimits) (*MemoryEntry, error)
	// DeleteMemoryEntry hard-deletes one of the user's entries (ErrNotFound
	// otherwise); with expectStatus set, only when the entry still has this
	// status (ErrMemoryConflict otherwise).
	DeleteMemoryEntry(ctx context.Context, login, id, expectStatus string) error
	// ListMemoryEntriesToBackfill returns the entries of every user flagged
	// metadata.legacy_backfill by migration 0014 (memory.BackfillLegacy).
	ListMemoryEntriesToBackfill(ctx context.Context) ([]*MemoryEntry, error)
	// ListMemoryUsers returns the users with active entries (consolidation).
	ListMemoryUsers(ctx context.Context) ([]string, error)
	// PurgeDeletedMemoryEntries hard-deletes entries soft-deleted before before.
	PurgeDeletedMemoryEntries(ctx context.Context, before time.Time) (int, error)
}
