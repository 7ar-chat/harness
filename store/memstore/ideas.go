package memstore

import (
	"context"
	"encoding/json"
	"sort"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/store"
)

// RecordIdeaUsage mirrors PG: the CHECK constraints, and a thread kept only when it is the user's.
func (s *Store) RecordIdeaUsage(_ context.Context, u store.IdeaUsage) (*store.IdeaUsage, error) {
	if badText(u.UserLogin, u.IdeaID, u.Surface) {
		return nil, ErrInvalidText
	}
	if n := utf8.RuneCountInString(u.IdeaID); n < 1 || n > 100 || utf8.RuneCountInString(u.Surface) > 64 ||
		(u.Action != store.IdeaSelected && u.Action != store.IdeaSubmitted) {
		return nil, store.ErrInvalidData
	}
	metadata := map[string]any{}
	if len(u.Metadata) > 0 {
		b, err := json.Marshal(store.CleanValue(u.Metadata))
		if err != nil {
			return nil, err
		}
		_ = json.Unmarshal(b, &metadata)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.threads[u.ThreadID]; !ok || t.UserLogin != u.UserLogin {
		u.ThreadID = ""
	}
	u.ID = uuid.NewString()
	u.Metadata = metadata
	u.CreatedAt = s.now()
	s.ideaUsage = append(s.ideaUsage, &u)
	cp := u
	return &cp, nil
}

// ListIdeaUsage returns a user's usage, newest first.
func (s *Store) ListIdeaUsage(_ context.Context, login string) ([]*store.IdeaUsage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*store.IdeaUsage
	for i := len(s.ideaUsage) - 1; i >= 0; i-- {
		if u := s.ideaUsage[i]; u.UserLogin == login {
			cp := *u
			out = append(out, &cp)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}
