// Package reqstore persists requirements (core/requirements). Mem keeps them in memory, File adds an atomically
// rewritten JSON file, PG shares them between processes (tables of migration 0590, applied by pgstore). One
// conformance suite runs against all three.
package reqstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/requirements"
)

type state struct {
	reqs     map[string]requirements.Requirement
	settings map[string]requirements.Settings
}

func (s state) clone() state {
	return state{reqs: maps.Clone(s.reqs), settings: maps.Clone(s.settings)}
}

// Mem is a requirements.Store in memory. It is safe for concurrent use.
type Mem struct {
	mu  sync.Mutex
	now func() time.Time
	st  state
	// save persists a new state before it replaces the current one; nil for a pure memory store.
	save func(state) error
}

var _ requirements.Store = (*Mem)(nil)

// NewMem returns an empty store; now (nil: time.Now) is the clock.
func NewMem(now func() time.Time) *Mem {
	if now == nil {
		now = time.Now
	}
	return &Mem{now: now, st: state{reqs: map[string]requirements.Requirement{}, settings: map[string]requirements.Settings{}}}
}

// mutate applies fn to a copy of the state and persists it when fn succeeds; a failure leaves the store as it was.
func (m *Mem) mutate(fn func(st *state) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.st.clone()
	if err := fn(&next); err != nil {
		return err
	}
	if m.save != nil {
		if err := m.save(next); err != nil {
			return err
		}
	}
	m.st = next
	return nil
}

func (m *Mem) Create(_ context.Context, r requirements.Requirement) (*requirements.Requirement, bool, error) {
	r, err := requirements.Prepare(r)
	if err != nil {
		return nil, false, err
	}
	var out requirements.Requirement
	created := true
	err = m.mutate(func(st *state) error {
		if key := r.Source.Key(); key != "" {
			for _, cur := range st.reqs {
				if cur.ProjectID == r.ProjectID && cur.Source.Key() == key {
					out, created = requirements.Clone(cur), false
					return errExisting
				}
			}
		}
		now := m.now()
		r.ID, r.CreatedAt, r.UpdatedAt = uuid.NewString(), now, now
		st.reqs[r.ID] = requirements.Clone(r)
		out = requirements.Clone(r)
		return nil
	})
	if errors.Is(err, errExisting) {
		return &out, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &out, created, nil
}

var errExisting = errors.New("existing")

func (m *Mem) Get(_ context.Context, id string) (*requirements.Requirement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.st.reqs[id]
	if !ok {
		return nil, requirements.ErrNotFound
	}
	r = requirements.Clone(r)
	return &r, nil
}

func (m *Mem) Update(_ context.Context, id string, p requirements.Patch) (*requirements.Requirement, error) {
	var out requirements.Requirement
	err := m.mutate(func(st *state) error {
		r, ok := st.reqs[id]
		if !ok {
			return requirements.ErrNotFound
		}
		if err := requirements.Apply(&r, p); err != nil {
			return err
		}
		r.UpdatedAt = m.now()
		st.reqs[id] = requirements.Clone(r)
		out = requirements.Clone(r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (m *Mem) Delete(_ context.Context, id string) error {
	return m.mutate(func(st *state) error {
		if _, ok := st.reqs[id]; !ok {
			return requirements.ErrNotFound
		}
		delete(st.reqs, id)
		return nil
	})
}

func (m *Mem) List(_ context.Context, f requirements.Filter) ([]requirements.Requirement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []requirements.Requirement{}
	for _, r := range m.st.reqs {
		if f.Match(r) {
			out = append(out, requirements.Clone(r))
		}
	}
	slices.SortFunc(out, func(a, b requirements.Requirement) int {
		switch {
		case requirements.Less(a, b):
			return -1
		case requirements.Less(b, a):
			return 1
		}
		return 0
	})
	limit := f.Limit
	if limit <= 0 {
		limit = 500
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Mem) GetSettings(_ context.Context, projectID string) (requirements.Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.st.settings[projectID]; ok {
		return s, nil
	}
	return requirements.Settings{ProjectID: projectID}, nil
}

func (m *Mem) PutSettings(_ context.Context, s requirements.Settings) error {
	if s.ProjectID == "" {
		return errors.Join(requirements.ErrInvalid, errors.New("settings belong to a project"))
	}
	return m.mutate(func(st *state) error {
		st.settings[s.ProjectID] = s
		return nil
	})
}

// ---- File ----

const fileVersion = 1

// File is a Mem persisted to a JSON file: every change rewrites the file atomically before it is applied. One
// process per file.
type File struct {
	*Mem
	path string
}

var _ requirements.Store = (*File)(nil)

type fileData struct {
	Version      int                        `json:"version"`
	Requirements []requirements.Requirement `json:"requirements"`
	Settings     []requirements.Settings    `json:"settings"`
}

// Open returns a store persisted to path. A missing file is an empty store; a file that cannot be read or
// parsed is an error and is left untouched.
func Open(path string, now func() time.Time) (*File, error) {
	f := &File{Mem: NewMem(now), path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("reqstore: create directory of %s: %w", path, err)
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("reqstore: read %s: %w", path, err)
	default:
		var d fileData
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, fmt.Errorf("reqstore: load %s: %w", path, err)
		}
		if d.Version != fileVersion {
			return nil, fmt.Errorf("reqstore: load %s: unsupported version %d (want %d)", path, d.Version, fileVersion)
		}
		for _, r := range d.Requirements {
			if r.ID == "" {
				return nil, fmt.Errorf("reqstore: load %s: requirement without an id", path)
			}
			f.st.reqs[r.ID] = r
		}
		for _, s := range d.Settings {
			f.st.settings[s.ProjectID] = s
		}
	}
	f.Mem.save = f.write
	return f, nil
}

func (f *File) write(st state) error {
	d := fileData{Version: fileVersion, Requirements: []requirements.Requirement{}, Settings: []requirements.Settings{}}
	for _, r := range st.reqs {
		d.Requirements = append(d.Requirements, r)
	}
	slices.SortFunc(d.Requirements, func(a, b requirements.Requirement) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	for _, s := range st.settings {
		d.Settings = append(d.Settings, s)
	}
	slices.SortFunc(d.Settings, func(a, b requirements.Settings) int {
		if a.ProjectID < b.ProjectID {
			return -1
		}
		if a.ProjectID > b.ProjectID {
			return 1
		}
		return 0
	})
	b, err := json.Marshal(&d)
	if err != nil {
		return fmt.Errorf("reqstore: encode %s: %w", f.path, err)
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("reqstore: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, f.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("reqstore: replace %s: %w", f.path, err)
	}
	return nil
}
