// Package rolestore persists roles (core/roles): the agent configurations of projects and users, with the last
// versions of each. Mem keeps them in memory, File adds an atomically rewritten JSON file, PG shares them
// between processes (tables of migration 0580, applied by pgstore). One conformance suite runs against all.
package rolestore

import (
	"context"
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/7ar-chat/harness/core/roles"
)

func key(scope roles.Scope, scopeID, name string) string {
	return string(scope) + "\x00" + scopeID + "\x00" + name
}

// state is everything a Mem holds. A change builds a new state; values are never mutated in place.
type state struct {
	roles map[string]roles.Role
	// history holds the past versions of each role, newest first.
	history map[string][]roles.Version
}

func (s state) clone() state {
	out := state{roles: make(map[string]roles.Role, len(s.roles)), history: make(map[string][]roles.Version, len(s.history))}
	for k, r := range s.roles {
		out.roles[k] = cloneRole(r)
	}
	for k, h := range s.history {
		c := make([]roles.Version, len(h))
		for i, v := range h {
			c[i] = v
			c[i].Role = cloneRole(v.Role)
		}
		out.history[k] = c
	}
	return out
}

func cloneRole(r roles.Role) roles.Role {
	r.AllowTools, r.DenyTools = slices.Clone(r.AllowTools), slices.Clone(r.DenyTools)
	r.Skills, r.MCP = slices.Clone(r.Skills), slices.Clone(r.MCP)
	return r
}

// Mem is a roles.Store in memory. It is safe for concurrent use.
type Mem struct {
	mu  sync.Mutex
	now func() time.Time
	st  state
	// save persists a new state before it replaces the current one; nil for a pure memory store.
	save func(state) error
}

var _ roles.Store = (*Mem)(nil)

// NewMem returns an empty store; now (nil: time.Now) is the clock.
func NewMem(now func() time.Time) *Mem {
	if now == nil {
		now = time.Now
	}
	return &Mem{now: now, st: state{roles: map[string]roles.Role{}, history: map[string][]roles.Version{}}}
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

// Create implements roles.Store.
func (m *Mem) Create(_ context.Context, r roles.Role, actor string) (*roles.Role, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	now := m.now().UTC().Truncate(time.Second)
	r.Version, r.Builtin, r.CreatedBy, r.UpdatedBy, r.CreatedAt, r.UpdatedAt = 1, false, actor, actor, now, now
	err := m.mutate(func(st *state) error {
		k := key(r.Scope, r.ScopeID, r.Name)
		if _, ok := st.roles[k]; ok {
			return roles.ErrConflict
		}
		st.roles[k] = cloneRole(r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// Update implements roles.Store.
func (m *Mem) Update(_ context.Context, r roles.Role, actor string) (*roles.Role, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	now := m.now().UTC().Truncate(time.Second)
	var out roles.Role
	err := m.mutate(func(st *state) error {
		k := key(r.Scope, r.ScopeID, r.Name)
		cur, ok := st.roles[k]
		if !ok {
			return roles.ErrNotFound
		}
		if r.Version != 0 && r.Version != cur.Version {
			return roles.ErrStale
		}
		h := append([]roles.Version{{Version: cur.Version, Role: cloneRole(cur), ChangedBy: cur.UpdatedBy, At: cur.UpdatedAt}}, st.history[k]...)
		if len(h) > roles.MaxHistory {
			h = h[:roles.MaxHistory]
		}
		st.history[k] = h
		r.Version, r.Builtin = cur.Version+1, false
		r.CreatedBy, r.CreatedAt, r.UpdatedBy, r.UpdatedAt = cur.CreatedBy, cur.CreatedAt, actor, now
		st.roles[k] = cloneRole(r)
		out = cloneRole(r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Get implements roles.Store.
func (m *Mem) Get(_ context.Context, scope roles.Scope, scopeID, name string) (*roles.Role, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.st.roles[key(scope, scopeID, name)]
	if !ok {
		return nil, roles.ErrNotFound
	}
	r = cloneRole(r)
	return &r, nil
}

// List implements roles.Store.
func (m *Mem) List(_ context.Context, scope roles.Scope, scopeID string) ([]roles.Role, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []roles.Role{}
	for _, r := range m.st.roles {
		if r.Scope == scope && r.ScopeID == scopeID {
			out = append(out, cloneRole(r))
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// History implements roles.Store.
func (m *Mem) History(_ context.Context, scope roles.Scope, scopeID, name string) ([]roles.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(scope, scopeID, name)
	if _, ok := m.st.roles[k]; !ok {
		return nil, roles.ErrNotFound
	}
	out := make([]roles.Version, 0, len(m.st.history[k]))
	for _, v := range m.st.history[k] {
		v.Role = cloneRole(v.Role)
		out = append(out, v)
	}
	return out, nil
}

// Delete implements roles.Store.
func (m *Mem) Delete(_ context.Context, scope roles.Scope, scopeID, name string) error {
	return m.mutate(func(st *state) error {
		k := key(scope, scopeID, name)
		if _, ok := st.roles[k]; !ok {
			return roles.ErrNotFound
		}
		delete(st.roles, k)
		delete(st.history, k)
		return nil
	})
}

// DeleteScope implements roles.Store.
func (m *Mem) DeleteScope(_ context.Context, scope roles.Scope, scopeID string) error {
	return m.mutate(func(st *state) error {
		for k, r := range st.roles {
			if r.Scope == scope && r.ScopeID == scopeID {
				delete(st.roles, k)
				delete(st.history, k)
			}
		}
		return nil
	})
}
