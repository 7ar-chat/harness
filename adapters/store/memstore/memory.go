package memstore

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/store"
)

// memory is the in-memory state of the memory entries.
type memory struct {
	entries  []*store.MemoryEntry
	disabled map[string]bool                            // users with memory disabled
	vectors  map[string]map[string]store.EntryEmbedding // login → entry id → embedding
}

func (s *Store) memInit() {
	if s.mem.disabled == nil {
		s.mem.disabled = map[string]bool{}
	}
}

func (s *Store) MemoryEnabled(_ context.Context, login string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.memInit()
	return !s.mem.disabled[login], nil
}

func (s *Store) SetMemoryEnabled(_ context.Context, login string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.memInit()
	s.mem.disabled[login] = !enabled
	return nil
}

func hasStatus(e *store.MemoryEntry, statuses []string) bool {
	if len(statuses) == 0 {
		return true
	}
	for _, st := range statuses {
		if e.Status == st {
			return true
		}
	}
	return false
}

func (s *Store) ListMemoryEntries(_ context.Context, login string, statuses ...string) ([]*store.MemoryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*store.MemoryEntry
	for _, e := range s.mem.entries {
		if e.UserLogin == login && hasStatus(e, statuses) {
			out = append(out, e.Clone())
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *Store) GetMemoryEntry(_ context.Context, login, id string) (*store.MemoryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.mem.entries {
		if e.UserLogin == login && e.ID == id {
			return e.Clone(), nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *Store) GetMemoryEntryByPath(_ context.Context, login, path string) (*store.MemoryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *store.MemoryEntry
	for _, e := range s.mem.entries {
		if e.UserLogin != login || e.Path != path {
			continue
		}
		if e.Status != store.MemoryDeleted {
			return e.Clone(), nil
		}
		if best == nil || e.UpdatedAt.After(best.UpdatedAt) {
			best = e
		}
	}
	if best == nil {
		return nil, store.ErrNotFound
	}
	return best.Clone(), nil
}

// cleanEntry returns the copy of e that Postgres would store: text sanitized like store.CleanText, and tags, metadata and
// proposal passed through the same JSON (jsonb) round trip, so a numeric metadata value comes back as float64 from every backend.
// It fails with store.ErrInvalidData on what the work_memory_entries constraints reject.
func cleanEntry(e *store.MemoryEntry) (*store.MemoryEntry, error) {
	cp := e.Clone()
	cp.Path, cp.Title, cp.Content = store.CleanText(cp.Path), store.CleanText(cp.Title), store.CleanText(cp.Content)
	for i, t := range cp.Tags {
		cp.Tags[i] = store.CleanText(t)
	}
	cp.Metadata = cleanMetadata(cp.Metadata)
	cp.Proposal = cleanProposal(cp.Proposal)
	switch cp.Status {
	case store.MemoryActive, store.MemoryPending, store.MemoryDeleted:
	default:
		return nil, store.ErrInvalidData
	}
	if n := len([]rune(cp.Path)); n < 1 || n > 512 {
		return nil, store.ErrInvalidData
	}
	if cp.Importance != nil && (*cp.Importance < 0 || *cp.Importance > 1) {
		return nil, store.ErrInvalidData
	}
	return cp, nil
}

// cleanMetadata is m as a jsonb column stores and returns it.
func cleanMetadata(m map[string]any) map[string]any {
	b, _ := json.Marshal(store.CleanValue(m))
	var out map[string]any
	if json.Unmarshal(store.CleanJSON(b), &out) != nil || out == nil {
		return map[string]any{}
	}
	return out
}

// cleanProposal is p as the proposal jsonb column stores and returns it.
func cleanProposal(p *store.MemoryProposal) *store.MemoryProposal {
	if p == nil {
		return nil
	}
	b, _ := json.Marshal(p)
	var v any
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	b, _ = json.Marshal(store.CleanValue(v))
	var out store.MemoryProposal
	if json.Unmarshal(store.CleanJSON(b), &out) != nil {
		return nil
	}
	if out.Tags == nil {
		out.Tags = []string{}
	}
	if out.Metadata == nil {
		out.Metadata = map[string]any{}
	}
	return &out
}

func (s *Store) pathClashLocked(e *store.MemoryEntry) bool {
	if e.Status == store.MemoryDeleted {
		return false
	}
	for _, o := range s.mem.entries {
		if o.UserLogin == e.UserLogin && o.ID != e.ID && o.Path == e.Path && o.Status != store.MemoryDeleted {
			return true
		}
	}
	return false
}

// fullLocked: the user already has max entries of status (≤ 0: no limit).
func (s *Store) fullLocked(login, status string, max int) bool {
	if max <= 0 {
		return false
	}
	n := 0
	for _, o := range s.mem.entries {
		if o.UserLogin == login && o.Status == status {
			n++
		}
	}
	return n >= max
}

func (s *Store) CreateMemoryEntry(_ context.Context, e *store.MemoryEntry, limits store.MemoryLimits) (*store.MemoryEntry, error) {
	cp, err := cleanEntry(e)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fullLocked(cp.UserLogin, cp.Status, limits.Of(cp.Status)) {
		return nil, store.ErrMemoryLimit
	}
	for _, o := range s.mem.entries {
		if o.ID == cp.ID {
			return nil, store.ErrInvalidData // primary key
		}
	}
	if s.pathClashLocked(cp) {
		return nil, store.ErrMemoryPathExists
	}
	if _, err := uuid.Parse(cp.SourceThreadID); err != nil {
		cp.SourceThreadID = ""
	}
	now := cp.CreatedAt
	if now.IsZero() {
		now = s.now()
	}
	cp.CreatedAt, cp.UpdatedAt, cp.ContentUpdatedAt, cp.DeletedAt, cp.Version = now, now, now, nil, 0
	s.mem.entries = append(s.mem.entries, cp)
	return cp.Clone(), nil
}

func (s *Store) UpdateMemoryEntry(_ context.Context, e *store.MemoryEntry, limits store.MemoryLimits) (*store.MemoryEntry, error) {
	cp, err := cleanEntry(e)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.mem.entries {
		if o.UserLogin != cp.UserLogin || o.ID != cp.ID {
			continue
		}
		if o.Version != cp.Version {
			return nil, store.ErrMemoryConflict
		}
		if o.Status != cp.Status && s.fullLocked(cp.UserLogin, cp.Status, limits.Of(cp.Status)) {
			return nil, store.ErrMemoryLimit
		}
		if s.pathClashLocked(cp) {
			return nil, store.ErrMemoryPathExists
		}
		o.Path, o.Title, o.Content, o.Tags, o.Importance = cp.Path, cp.Title, cp.Content, cp.Tags, cp.Importance
		o.Metadata, o.Status, o.ContentHash = cp.Metadata, cp.Status, cp.ContentHash
		o.UpdatedAt, o.ContentUpdatedAt, o.DeletedAt, o.Proposal = cp.UpdatedAt, cp.ContentUpdatedAt, cp.DeletedAt, cp.Proposal
		o.Version++
		return o.Clone(), nil
	}
	return nil, store.ErrNotFound
}

func (s *Store) DeleteMemoryEntry(_ context.Context, login, id, expectStatus string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.mem.entries {
		if e.UserLogin == login && e.ID == id {
			if expectStatus != "" && e.Status != expectStatus {
				return store.ErrMemoryConflict
			}
			s.mem.entries = append(s.mem.entries[:i], s.mem.entries[i+1:]...)
			return nil
		}
	}
	return store.ErrNotFound
}

func (s *Store) ListMemoryUsers(_ context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, e := range s.mem.entries {
		if e.Status == store.MemoryActive && !seen[e.UserLogin] {
			seen[e.UserLogin] = true
			out = append(out, e.UserLogin)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *Store) PurgeDeletedMemoryEntries(_ context.Context, before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.mem.entries[:0]
	n := 0
	for _, e := range s.mem.entries {
		if e.Status == store.MemoryDeleted && e.DeletedAt != nil && e.DeletedAt.Before(before) {
			n++
			continue
		}
		kept = append(kept, e)
	}
	s.mem.entries = kept
	return n, nil
}

// SetMemoryEntryTimes overrides an entry's timestamps (tests: recency, retention).
func (s *Store) SetMemoryEntryTimes(id string, updatedAt time.Time, deletedAt *time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.mem.entries {
		if e.ID == id {
			e.UpdatedAt, e.ContentUpdatedAt = updatedAt, updatedAt
			if deletedAt != nil {
				t := *deletedAt
				e.DeletedAt = &t
			}
		}
	}
}
