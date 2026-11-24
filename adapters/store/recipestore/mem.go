// Package recipestore persists recipes (core/recipes): the templates people share on an instance, in a project or
// for themselves, with the last versions of each and how often each was used. Mem keeps them in memory, File adds an
// atomically rewritten JSON file, PG shares them between processes (tables of migration 1100, applied by pgstore).
// One conformance suite runs against all.
package recipestore

import (
	"context"
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/recipes"
)

type state struct {
	recipes map[string]recipes.Recipe
	// history holds the past versions of each recipe, newest first.
	history map[string][]recipes.Version
	uses    map[string]int
}

func newState() state {
	return state{recipes: map[string]recipes.Recipe{}, history: map[string][]recipes.Version{}, uses: map[string]int{}}
}

func cloneRecipe(r recipes.Recipe) recipes.Recipe {
	ps := make([]recipes.Param, len(r.Params))
	for i, p := range r.Params {
		p.Choices = slices.Clone(p.Choices)
		ps[i] = p
	}
	r.Params = ps
	if r.Body.Match != nil {
		m := make(map[string]string, len(r.Body.Match))
		for k, v := range r.Body.Match {
			m[k] = v
		}
		r.Body.Match = m
	}
	return r
}

func (s state) clone() state {
	out := newState()
	for k, r := range s.recipes {
		out.recipes[k] = cloneRecipe(r)
	}
	for k, h := range s.history {
		c := make([]recipes.Version, len(h))
		for i, v := range h {
			c[i] = v
			c[i].Recipe = cloneRecipe(v.Recipe)
		}
		out.history[k] = c
	}
	for k, n := range s.uses {
		out.uses[k] = n
	}
	return out
}

// Mem is a recipes.Store in memory. It is safe for concurrent use.
type Mem struct {
	mu  sync.Mutex
	now func() time.Time
	st  state
	// save persists a new state before it replaces the current one; nil for a pure memory store.
	save func(state) error
}

var _ recipes.Store = (*Mem)(nil)

// NewMem returns an empty store; now (nil: time.Now) is the clock.
func NewMem(now func() time.Time) *Mem {
	if now == nil {
		now = time.Now
	}
	return &Mem{now: now, st: newState()}
}

func (m *Mem) mutate(fn func(st *state) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.st.clone()
	if err := fn(&next); err != nil {
		return err
	}
	if reflect.DeepEqual(m.st, next) {
		return nil
	}
	if m.save != nil {
		if err := m.save(next); err != nil {
			return err
		}
	}
	m.st = next
	return nil
}

// prepare validates r and clears what the store owns.
func prepare(r recipes.Recipe) (recipes.Recipe, error) {
	if r.Scope != recipes.ScopeInstance && r.Scope != recipes.ScopeProject && r.Scope != recipes.ScopePersonal {
		return r, recipes.ErrInvalid
	}
	if err := r.Validate(); err != nil {
		return r, err
	}
	r.Uses = 0
	return cloneRecipe(r), nil
}

// Create implements recipes.Store.
func (m *Mem) Create(_ context.Context, r recipes.Recipe, actor string) (*recipes.Recipe, error) {
	r, err := prepare(r)
	if err != nil {
		return nil, err
	}
	now := m.now().UTC().Truncate(time.Second)
	r.ID, r.Version, r.Author, r.CreatedAt, r.UpdatedAt = "r_"+uuid.NewString(), 1, actor, now, now
	if err := m.mutate(func(st *state) error { st.recipes[r.ID] = cloneRecipe(r); return nil }); err != nil {
		return nil, err
	}
	return &r, nil
}

// Update implements recipes.Store.
func (m *Mem) Update(_ context.Context, r recipes.Recipe, actor string) (*recipes.Recipe, error) {
	id, version := r.ID, r.Version
	if r.Scope == "" { // the scope of a stored recipe is not editable; validation needs one
		r.Scope = recipes.ScopePersonal
	}
	r, err := prepare(r)
	if err != nil {
		return nil, err
	}
	now := m.now().UTC().Truncate(time.Second)
	var out recipes.Recipe
	err = m.mutate(func(st *state) error {
		cur, ok := st.recipes[id]
		if !ok {
			return recipes.ErrNotFound
		}
		if version != 0 && version != cur.Version {
			return recipes.ErrStale
		}
		h := append([]recipes.Version{{Version: cur.Version, Recipe: cloneRecipe(cur), ChangedBy: actor, At: cur.UpdatedAt}}, st.history[id]...)
		if len(h) > recipes.MaxHistory {
			h = h[:recipes.MaxHistory]
		}
		st.history[id] = h
		// scope, author and creation are not editable
		r.ID, r.Scope, r.ScopeID, r.Author, r.CreatedAt = id, cur.Scope, cur.ScopeID, cur.Author, cur.CreatedAt
		r.Version, r.UpdatedAt = cur.Version+1, now
		st.recipes[id] = cloneRecipe(r)
		out = cloneRecipe(r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Get implements recipes.Store.
func (m *Mem) Get(_ context.Context, id string) (*recipes.Recipe, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.st.recipes[id]
	if !ok {
		return nil, recipes.ErrNotFound
	}
	r = cloneRecipe(r)
	return &r, nil
}

// List implements recipes.Store.
func (m *Mem) List(_ context.Context, scope recipes.Scope, scopeID string) ([]recipes.Recipe, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []recipes.Recipe{}
	for _, r := range m.st.recipes {
		if scope == "" || r.Scope == scope && r.ScopeID == scopeID {
			out = append(out, cloneRecipe(r))
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Name != out[b].Name {
			return out[a].Name < out[b].Name
		}
		return out[a].ID < out[b].ID
	})
	return out, nil
}

// History implements recipes.Store.
func (m *Mem) History(_ context.Context, id string) ([]recipes.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.st.recipes[id]; !ok {
		return nil, recipes.ErrNotFound
	}
	out := make([]recipes.Version, 0, len(m.st.history[id]))
	for _, v := range m.st.history[id] {
		v.Recipe = cloneRecipe(v.Recipe)
		out = append(out, v)
	}
	return out, nil
}

// Delete implements recipes.Store.
func (m *Mem) Delete(_ context.Context, id string) error {
	return m.mutate(func(st *state) error {
		if _, ok := st.recipes[id]; !ok {
			return recipes.ErrNotFound
		}
		delete(st.recipes, id)
		delete(st.history, id)
		delete(st.uses, id)
		return nil
	})
}

// DeleteScope implements recipes.Store.
func (m *Mem) DeleteScope(_ context.Context, scope recipes.Scope, scopeID string) error {
	return m.mutate(func(st *state) error {
		for id, r := range st.recipes {
			if r.Scope == scope && r.ScopeID == scopeID {
				delete(st.recipes, id)
				delete(st.history, id)
				delete(st.uses, id)
			}
		}
		return nil
	})
}

// AddUse implements recipes.Store.
func (m *Mem) AddUse(_ context.Context, id string) error {
	return m.mutate(func(st *state) error { st.uses[id]++; return nil })
}

// Uses implements recipes.Store.
func (m *Mem) Uses(_ context.Context) (map[string]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int, len(m.st.uses))
	for k, n := range m.st.uses {
		out[k] = n
	}
	return out, nil
}
