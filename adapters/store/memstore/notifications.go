package memstore

import (
	"context"

	"github.com/7ar-chat/harness/core/store"
)

// notifyViewLocked sets LastRun and Unread (harness v3 §8) like the PG query.
func (s *Store) notifyViewLocked(t *store.Thread) {
	t.LastRun, t.Unread = nil, false
	seen := t.CreatedAt
	if t.LastSeenAt != nil {
		seen = *t.LastSeenAt
	}
	var last *store.Run
	for _, r := range s.runs {
		if r.ThreadID != t.ID {
			continue
		}
		if last == nil || r.CreatedAt.After(last.CreatedAt) || (r.CreatedAt.Equal(last.CreatedAt) && r.ID > last.ID) {
			last = r
		}
		if r.FinishedAt != nil && r.FinishedAt.After(seen) {
			t.Unread = true
		}
	}
	if last != nil {
		t.LastRun = &store.LastRun{ID: last.ID, Status: last.Status, FinishedAt: last.FinishedAt}
	}
}

func (s *Store) MarkThreadSeen(_ context.Context, login, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.ownedLocked(login, id)
	if err != nil {
		return err
	}
	now := s.now()
	t.LastSeenAt = &now
	return nil
}

func (s *Store) CountUnreadThreads(_ context.Context, login string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, t := range s.threads {
		if !t.deleted && t.UserLogin == login && s.viewLocked(t).Unread {
			n++
		}
	}
	return n, nil
}

func (s *Store) SearchUnreadThreads(ctx context.Context, login, search string, limit int, before *store.ThreadCursor) ([]*store.Thread, error) {
	all, err := s.SearchThreads(ctx, login, search, 1<<30, before)
	if err != nil {
		return nil, err
	}
	out := make([]*store.Thread, 0, limit)
	for _, t := range all {
		if t.Unread && len(out) < limit {
			out = append(out, t)
		}
	}
	return out, nil
}
