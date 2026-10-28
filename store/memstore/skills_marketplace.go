package memstore

import (
	"context"
	"sort"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/store"
)

// marketplace mimics migration 0015: global skill sources and sync runs
// (guarded by catalog.mu, like the global skills they reference).
type marketplace struct {
	sources map[string]*store.MarketplaceSource // by global skill id
	runs    map[string]*store.MarketplaceRun
}

func (c *catalog) m() *marketplace {
	if c.market == nil {
		c.market = &marketplace{sources: map[string]*store.MarketplaceSource{}, runs: map[string]*store.MarketplaceRun{}}
	}
	return c.market
}

func copyRun(r *store.MarketplaceRun) *store.MarketplaceRun {
	cp := *r
	cp.Warnings = append([]store.MarketplaceIssue(nil), r.Warnings...)
	cp.Errors = append([]store.MarketplaceIssue(nil), r.Errors...)
	return &cp
}

// MarketplaceSources implements store.SkillMarketplaceStore.
func (s *Store) MarketplaceSources(_ context.Context) ([]*store.MarketplaceSource, error) {
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*store.MarketplaceSource
	for id, src := range c.m().sources {
		if _, ok := c.globals[id]; !ok { // ON DELETE CASCADE
			delete(c.m().sources, id)
			continue
		}
		cp := *src
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SourcePath < out[j].SourcePath })
	return out, nil
}

// UpsertMarketplaceSource implements store.SkillMarketplaceStore.
func (s *Store) UpsertMarketplaceSource(_ context.Context, src store.MarketplaceSource) error {
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.globals[src.GlobalSkillID]; !ok {
		return store.ErrNotFound
	}
	src.LastSyncedAt = s.now()
	c.m().sources[src.GlobalSkillID] = &src
	return nil
}

// UpdateGlobalSkillInfo implements store.SkillMarketplaceStore.
func (s *Store) UpdateGlobalSkillInfo(_ context.Context, id, displayName, description string) error {
	if badText(displayName, description) {
		return ErrInvalidText
	}
	return s.updateGlobal(id, func(g *store.GlobalSkill) { g.DisplayName, g.Description = displayName, description })
}

// CreateMarketplaceRun implements store.SkillMarketplaceStore.
func (s *Store) CreateMarketplaceRun(_ context.Context, r *store.MarketplaceRun) error {
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	r.ID, r.StartedAt = uuid.NewString(), s.now()
	if r.Status == "" {
		r.Status = store.MarketplaceRunRunning
	}
	c.m().runs[r.ID] = copyRun(r)
	return nil
}

// FinishMarketplaceRun implements store.SkillMarketplaceStore.
func (s *Store) FinishMarketplaceRun(_ context.Context, r *store.MarketplaceRun) error {
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, ok := c.m().runs[r.ID]
	if !ok {
		return store.ErrNotFound
	}
	now := s.now()
	r.StartedAt, r.FinishedAt = cur.StartedAt, &now
	c.m().runs[r.ID] = copyRun(r)
	return nil
}

// ListMarketplaceRuns implements store.SkillMarketplaceStore.
func (s *Store) ListMarketplaceRuns(_ context.Context, limit int) ([]*store.MarketplaceRun, error) {
	c := s.c()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*store.MarketplaceRun, 0, len(c.m().runs))
	for _, r := range c.m().runs {
		out = append(out, copyRun(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

var _ store.SkillMarketplaceStore = (*Store)(nil)
