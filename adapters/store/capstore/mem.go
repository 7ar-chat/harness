// Package capstore persists the MCP marketplace (core/capabilities): the catalog and the installs of
// projects and users. Mem keeps them in memory, File adds an atomically rewritten JSON file, PG shares them
// between processes (tables of migration 0500, applied by pgstore). One conformance suite runs against all.
package capstore

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/capabilities"
)

// state is everything a Mem holds. Values are never mutated in place: a change builds a new state.
type state struct {
	catalog  map[string]capabilities.Entry
	installs map[string]capabilities.Install
}

func (s state) clone() state {
	return state{catalog: maps.Clone(s.catalog), installs: maps.Clone(s.installs)}
}

// Mem is a capabilities.Store in memory. It is safe for concurrent use.
type Mem struct {
	mu  sync.Mutex
	now func() time.Time
	st  state
	// save persists a new state before it replaces the current one; nil for a pure memory store.
	save func(state) error
}

var _ capabilities.Store = (*Mem)(nil)

// NewMem returns an empty store; now (nil: time.Now) is the clock.
func NewMem(now func() time.Time) *Mem {
	if now == nil {
		now = time.Now
	}
	return &Mem{now: now, st: state{catalog: map[string]capabilities.Entry{}, installs: map[string]capabilities.Install{}}}
}

// mutate applies fn to a copy of the state and, when fn succeeds and changed something, persists the copy
// and makes it current. A failure leaves the store as it was.
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

func (m *Mem) SeedCatalog(_ context.Context, entries []capabilities.Entry) error {
	want, err := seedSet(entries)
	if err != nil {
		return err
	}
	return m.mutate(func(st *state) error {
		for id, cur := range st.catalog {
			if _, ok := want[id]; !ok && cur.Builtin {
				delete(st.catalog, id)
			}
		}
		for id, e := range want {
			if cur, ok := st.catalog[id]; ok && !cur.Builtin {
				continue
			}
			st.catalog[id] = e
		}
		return nil
	})
}

func (m *Mem) ListCatalog(context.Context) ([]capabilities.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]capabilities.Entry, 0, len(m.st.catalog))
	for _, e := range m.st.catalog {
		out = append(out, cloneEntry(e))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}

func (m *Mem) GetCatalogEntry(_ context.Context, id string) (*capabilities.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.st.catalog[id]
	if !ok {
		return nil, capabilities.ErrNotFound
	}
	e = cloneEntry(e)
	return &e, nil
}

func (m *Mem) PutInstall(_ context.Context, in capabilities.Install) (*capabilities.Install, error) {
	in, err := prepare(in)
	if err != nil {
		return nil, err
	}
	var out capabilities.Install
	err = m.mutate(func(st *state) error {
		var cur *capabilities.Install
		switch {
		case in.ID != "":
			c, ok := st.installs[in.ID]
			if !ok || c.Scope != in.Scope || c.ScopeID != in.ScopeID || c.CatalogID != in.CatalogID {
				return capabilities.ErrNotFound
			}
			cur = &c
		case in.CatalogID != "":
			for _, c := range st.installs {
				if c.Scope == in.Scope && c.ScopeID == in.ScopeID && c.CatalogID == in.CatalogID {
					cur = &c
					break
				}
			}
		}
		if in.CatalogID == "" {
			if _, ok := st.catalog[in.Name]; ok {
				return fmt.Errorf("%w: %q is a catalog entry", capabilities.ErrConflict, in.Name)
			}
		}
		for id, o := range st.installs {
			if o.Scope == in.Scope && o.ScopeID == in.ScopeID && o.Name == in.Name && (cur == nil || id != cur.ID) {
				return fmt.Errorf("%w: %q", capabilities.ErrConflict, in.Name)
			}
		}
		now := m.now()
		if cur != nil {
			in.ID, in.CreatedBy, in.CreatedAt = cur.ID, cur.CreatedBy, cur.CreatedAt
		} else {
			in.ID, in.CreatedAt = uuid.NewString(), now
		}
		in.UpdatedAt = now
		st.installs[in.ID] = in
		out = cloneInstall(in)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (m *Mem) GetInstall(_ context.Context, id string) (*capabilities.Install, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in, ok := m.st.installs[id]
	if !ok {
		return nil, capabilities.ErrNotFound
	}
	in = cloneInstall(in)
	return &in, nil
}

func (m *Mem) ListInstalls(_ context.Context, scope capabilities.Scope, scopeID string) ([]capabilities.Install, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []capabilities.Install
	for _, in := range m.st.installs {
		if in.Scope == scope && in.ScopeID == scopeID {
			out = append(out, cloneInstall(in))
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

func (m *Mem) DeleteInstall(_ context.Context, id string) error {
	return m.mutate(func(st *state) error {
		if _, ok := st.installs[id]; !ok {
			return capabilities.ErrNotFound
		}
		delete(st.installs, id)
		return nil
	})
}

func (m *Mem) DeleteInstalls(_ context.Context, scope capabilities.Scope, scopeID string) error {
	return m.mutate(func(st *state) error {
		for id, in := range st.installs {
			if in.Scope == scope && in.ScopeID == scopeID {
				delete(st.installs, id)
			}
		}
		return nil
	})
}

// seedSet validates the fixture and returns it keyed by id, marked builtin.
func seedSet(entries []capabilities.Entry) (map[string]capabilities.Entry, error) {
	want := make(map[string]capabilities.Entry, len(entries))
	for _, e := range entries {
		if e.ID == "" {
			return nil, fmt.Errorf("capstore: catalog entry %q has no id", e.Name)
		}
		if _, dup := want[e.ID]; dup {
			return nil, fmt.Errorf("capstore: catalog entry %q listed twice", e.ID)
		}
		e = cloneEntry(e)
		e.Builtin = true
		want[e.ID] = e
	}
	return want, nil
}

// prepare validates an install and returns a normalised private copy of it. A catalog install is named by
// its entry; sorting SecretKeys and dropping empty slices makes every backend return the same shape.
func prepare(in capabilities.Install) (capabilities.Install, error) {
	if in.Scope != capabilities.ScopeProject && in.Scope != capabilities.ScopeUser {
		return in, fmt.Errorf("capstore: unknown scope %q", in.Scope)
	}
	if in.ScopeID == "" {
		return in, fmt.Errorf("capstore: install without a scope id")
	}
	if in.CatalogID != "" {
		in.Name = in.CatalogID
	} else if in.Name == "" {
		return in, fmt.Errorf("capstore: custom install without a name")
	}
	in = cloneInstall(in)
	slices.Sort(in.SecretKeys)
	in.SecretKeys = slices.Compact(in.SecretKeys)
	return in, nil
}

func nilIfEmpty[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return slices.Clone(s)
}

func cloneInstall(in capabilities.Install) capabilities.Install {
	in.Tools = nilIfEmpty(in.Tools)
	in.Secrets = nilIfEmpty(in.Secrets)
	in.SecretKeys = nilIfEmpty(in.SecretKeys)
	return in
}

func cloneEntry(e capabilities.Entry) capabilities.Entry {
	if len(e.Headers) == 0 {
		e.Headers = nil
	} else {
		e.Headers = maps.Clone(e.Headers)
	}
	e.Fields = nilIfEmpty(e.Fields)
	e.Tools = nilIfEmpty(e.Tools)
	return e
}
