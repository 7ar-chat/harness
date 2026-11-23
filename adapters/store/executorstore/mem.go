// Package executorstore persists the executors a project defined (core/executors). Mem keeps them in memory, File
// adds an atomically rewritten JSON file, PG shares them between processes (table of migration 0900, applied by
// pgstore). One conformance suite runs against all.
package executorstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/7ar-chat/harness/core/executors"
)

func key(projectID, name string) string { return projectID + "\x00" + name }

func clone(e executors.Executor) executors.Executor {
	e.CredentialEnv = slices.Clone(e.CredentialEnv)
	if e.ModeFlags != nil {
		m := make(map[executors.Level]string, len(e.ModeFlags))
		for k, v := range e.ModeFlags {
			m[k] = v
		}
		e.ModeFlags = m
	}
	return e
}

// Mem is an executors.Store in memory, safe for concurrent use.
type Mem struct {
	mu   sync.Mutex
	now  func() time.Time
	m    map[string]executors.Executor
	save func(map[string]executors.Executor) error
}

var _ executors.Store = (*Mem)(nil)

// NewMem returns an empty store; now (nil: time.Now) is the clock.
func NewMem(now func() time.Time) *Mem {
	if now == nil {
		now = time.Now
	}
	return &Mem{now: now, m: map[string]executors.Executor{}}
}

func (s *Mem) persist(next map[string]executors.Executor) error {
	if s.save != nil {
		if err := s.save(next); err != nil {
			return err
		}
	}
	s.m = next
	return nil
}

func (s *Mem) copyMap() map[string]executors.Executor {
	out := make(map[string]executors.Executor, len(s.m)+1)
	for k, v := range s.m {
		out[k] = v
	}
	return out
}

// Create implements executors.Store.
func (s *Mem) Create(_ context.Context, e executors.Executor, actor string) (*executors.Executor, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(e.ProjectID, e.Name)
	if _, ok := s.m[k]; ok {
		return nil, executors.ErrConflict
	}
	n := 0
	for _, x := range s.m {
		if x.ProjectID == e.ProjectID {
			n++
		}
	}
	if n >= executors.MaxListed {
		return nil, fmt.Errorf("%w: too many executors in the project", executors.ErrInvalid)
	}
	now := s.now().UTC().Truncate(time.Second)
	e.CreatedBy, e.CreatedAt, e.UpdatedAt = actor, now, now
	next := s.copyMap()
	next[k] = clone(e)
	if err := s.persist(next); err != nil {
		return nil, err
	}
	return &e, nil
}

// Update implements executors.Store.
func (s *Mem) Update(_ context.Context, e executors.Executor, _ string) (*executors.Executor, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(e.ProjectID, e.Name)
	cur, ok := s.m[k]
	if !ok {
		return nil, executors.ErrNotFound
	}
	e.CreatedBy, e.CreatedAt = cur.CreatedBy, cur.CreatedAt
	e.UpdatedAt = s.now().UTC().Truncate(time.Second)
	next := s.copyMap()
	next[k] = clone(e)
	if err := s.persist(next); err != nil {
		return nil, err
	}
	return &e, nil
}

// Get implements executors.Store.
func (s *Mem) Get(_ context.Context, projectID, name string) (*executors.Executor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[key(projectID, name)]
	if !ok {
		return nil, executors.ErrNotFound
	}
	e = clone(e)
	return &e, nil
}

// List implements executors.Store.
func (s *Mem) List(_ context.Context, projectID string) ([]executors.Executor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []executors.Executor{}
	for _, e := range s.m {
		if e.ProjectID == projectID {
			out = append(out, clone(e))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Delete implements executors.Store.
func (s *Mem) Delete(_ context.Context, projectID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(projectID, name)
	if _, ok := s.m[k]; !ok {
		return executors.ErrNotFound
	}
	next := s.copyMap()
	delete(next, k)
	return s.persist(next)
}

// DeleteProject implements executors.Store.
func (s *Mem) DeleteProject(_ context.Context, projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.copyMap()
	for k, e := range next {
		if e.ProjectID == projectID {
			delete(next, k)
		}
	}
	return s.persist(next)
}

// fileVersion is the schema of the persisted file; Open refuses any other.
const fileVersion = 1

// File is a Mem persisted to a JSON file: every change rewrites the file atomically before it is applied. One
// process per file.
type File struct {
	*Mem
	path string
}

var _ executors.Store = (*File)(nil)

type fileData struct {
	Version   int                  `json:"version"`
	Executors []executors.Executor `json:"executors"`
}

// Open returns a store persisted to path, loading the file if it exists.
func Open(path string, now func() time.Time) (*File, error) {
	f := &File{Mem: NewMem(now), path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("executorstore: create directory of %s: %w", path, err)
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("executorstore: read %s: %w", path, err)
	default:
		var d fileData
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, fmt.Errorf("executorstore: load %s: %w", path, err)
		}
		if d.Version != fileVersion {
			return nil, fmt.Errorf("executorstore: load %s: unsupported version %d (want %d)", path, d.Version, fileVersion)
		}
		for _, e := range d.Executors {
			f.m[key(e.ProjectID, e.Name)] = e
		}
	}
	f.Mem.save = f.write
	return f, nil
}

func (f *File) write(m map[string]executors.Executor) error {
	d := fileData{Version: fileVersion, Executors: make([]executors.Executor, 0, len(m))}
	for _, e := range m {
		d.Executors = append(d.Executors, e)
	}
	sort.Slice(d.Executors, func(a, b int) bool {
		return key(d.Executors[a].ProjectID, d.Executors[a].Name) < key(d.Executors[b].ProjectID, d.Executors[b].Name)
	})
	b, err := json.Marshal(&d)
	if err != nil {
		return fmt.Errorf("executorstore: encode %s: %w", f.path, err)
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("executorstore: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, f.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("executorstore: replace %s: %w", f.path, err)
	}
	return nil
}
