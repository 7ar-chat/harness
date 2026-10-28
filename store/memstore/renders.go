package memstore

import (
	"context"
	"slices"

	"github.com/GhaziBenDahmane/harness/store"
)

func (s *Store) PutRender(_ context.Context, r *store.Render) error {
	if badText(r.SourcePath, r.Label) {
		return ErrInvalidText
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.threads[r.ThreadID]; !ok {
		return store.ErrNotFound
	}
	if s.renders == nil {
		s.renders = map[string]*store.Render{}
	}
	key := r.ThreadID + "\x00" + r.RenderID
	now := s.now()
	cp := *r
	cp.CreatedAt, cp.UpdatedAt = now, now
	if old := s.renders[key]; old != nil {
		cp.CreatedAt = old.CreatedAt
		cp.Pages = append(slices.Clone(old.Pages), r.Pages...)
	} else {
		cp.Pages = slices.Clone(r.Pages)
	}
	slices.Sort(cp.Pages)
	cp.Pages = slices.Compact(cp.Pages)
	s.renders[key] = &cp
	return nil
}

func (s *Store) GetRender(_ context.Context, threadID, renderID string) (*store.Render, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.renders[threadID+"\x00"+renderID]
	if r == nil {
		return nil, store.ErrNotFound
	}
	cp := *r
	cp.Pages = slices.Clone(r.Pages)
	return &cp, nil
}
