// Package workflowstore persists workflows and their runs (core/workflows). Mem keeps them in memory, File adds
// an atomically rewritten JSON file, PG shares them between processes (tables of migration 0600, applied by
// pgstore). One conformance suite runs against all three.
package workflowstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/workflows"
)

// state is everything a Mem holds. A change builds a new state; values are never mutated in place.
type state struct {
	Wfs     map[string]workflows.Workflow  `json:"workflows"`
	History map[string][]workflows.Version `json:"history"`
	Runs    map[string]workflows.Run       `json:"runs"`
}

func newState() state {
	return state{Wfs: map[string]workflows.Workflow{}, History: map[string][]workflows.Version{}, Runs: map[string]workflows.Run{}}
}

// copyOf deep-copies v through JSON: workflows and runs are plain data.
func copyOf[T any](v T) T {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("workflowstore: encode: %v", err))
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		panic(fmt.Sprintf("workflowstore: decode: %v", err))
	}
	return out
}

func (s state) clone() state { return copyOf(s).normalized() }

func (s state) normalized() state {
	if s.Wfs == nil {
		s.Wfs = map[string]workflows.Workflow{}
	}
	if s.History == nil {
		s.History = map[string][]workflows.Version{}
	}
	if s.Runs == nil {
		s.Runs = map[string]workflows.Run{}
	}
	return s
}

// Mem is a workflows.Store in memory. It is safe for concurrent use.
type Mem struct {
	mu  sync.Mutex
	now func() time.Time
	st  state
	// save persists a new state before it replaces the current one; nil for a pure memory store.
	save func(state) error
}

var _ workflows.Store = (*Mem)(nil)

// NewMem returns an empty store; now (nil: time.Now) is the clock.
func NewMem(now func() time.Time) *Mem {
	if now == nil {
		now = time.Now
	}
	return &Mem{now: now, st: newState()}
}

func (m *Mem) stamp() time.Time { return m.now().UTC().Truncate(time.Second) }

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

func nameTaken(st *state, projectID, name, except string) bool {
	for id, w := range st.Wfs {
		if id != except && w.ProjectID == projectID && w.Name == name {
			return true
		}
	}
	return false
}

// Create implements workflows.Store.
func (m *Mem) Create(_ context.Context, w workflows.Workflow, actor string) (*workflows.Workflow, error) {
	if err := prepare(&w); err != nil {
		return nil, err
	}
	if w.ID == "" {
		w.ID = uuid.NewString()
	}
	now := m.stamp()
	w.Version, w.CreatedBy, w.UpdatedBy, w.CreatedAt, w.UpdatedAt = 1, actor, actor, now, now
	err := m.mutate(func(st *state) error {
		if _, ok := st.Wfs[w.ID]; ok || nameTaken(st, w.ProjectID, w.Name, "") {
			return workflows.ErrConflict
		}
		st.Wfs[w.ID] = copyOf(w)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// prepare checks what the store itself relies on; the graph is the caller's to validate.
func prepare(w *workflows.Workflow) error {
	if w.ProjectID == "" || w.Name == "" {
		return fmt.Errorf("%w: a workflow needs a project and a name", workflows.ErrInvalid)
	}
	if w.Status == "" {
		w.Status = workflows.StatusDraft
	}
	if w.Status != workflows.StatusDraft && w.Status != workflows.StatusPublished {
		return fmt.Errorf("%w: unknown status %q", workflows.ErrInvalid, w.Status)
	}
	if w.Nodes == nil {
		w.Nodes = []workflows.Node{}
	}
	return nil
}

// Update implements workflows.Store.
func (m *Mem) Update(_ context.Context, w workflows.Workflow, actor string) (*workflows.Workflow, error) {
	if err := prepare(&w); err != nil {
		return nil, err
	}
	now := m.stamp()
	var out workflows.Workflow
	err := m.mutate(func(st *state) error {
		cur, ok := st.Wfs[w.ID]
		if !ok {
			return workflows.ErrNotFound
		}
		if w.Version != 0 && w.Version != cur.Version {
			return workflows.ErrStale
		}
		if nameTaken(st, cur.ProjectID, w.Name, w.ID) {
			return workflows.ErrConflict
		}
		h := append([]workflows.Version{{Version: cur.Version, Workflow: cur, ChangedBy: cur.UpdatedBy, At: cur.UpdatedAt}}, st.History[w.ID]...)
		if len(h) > workflows.MaxHistory {
			h = h[:workflows.MaxHistory]
		}
		st.History[w.ID] = h
		w.ProjectID, w.Template = cur.ProjectID, cur.Template
		w.Version = cur.Version + 1
		w.CreatedBy, w.CreatedAt, w.UpdatedBy, w.UpdatedAt = cur.CreatedBy, cur.CreatedAt, actor, now
		st.Wfs[w.ID] = copyOf(w)
		out = copyOf(w)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Get implements workflows.Store.
func (m *Mem) Get(_ context.Context, id string) (*workflows.Workflow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.st.Wfs[id]
	if !ok {
		return nil, workflows.ErrNotFound
	}
	w = copyOf(w)
	return &w, nil
}

// List implements workflows.Store.
func (m *Mem) List(_ context.Context, projectID string) ([]workflows.Workflow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []workflows.Workflow{}
	for _, w := range m.st.Wfs {
		if w.ProjectID == projectID {
			out = append(out, copyOf(w))
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// History implements workflows.Store.
func (m *Mem) History(_ context.Context, id string) ([]workflows.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.st.Wfs[id]; !ok {
		return nil, workflows.ErrNotFound
	}
	return copyOf(append([]workflows.Version{}, m.st.History[id]...)), nil
}

// Delete implements workflows.Store.
func (m *Mem) Delete(_ context.Context, id string) error {
	return m.mutate(func(st *state) error {
		if _, ok := st.Wfs[id]; !ok {
			return workflows.ErrNotFound
		}
		delete(st.Wfs, id)
		delete(st.History, id)
		return nil
	})
}

// DeleteProject implements workflows.Store.
func (m *Mem) DeleteProject(_ context.Context, projectID string) error {
	return m.mutate(func(st *state) error {
		for id, w := range st.Wfs {
			if w.ProjectID == projectID {
				delete(st.Wfs, id)
				delete(st.History, id)
			}
		}
		for id, r := range st.Runs {
			if r.ProjectID == projectID {
				delete(st.Runs, id)
			}
		}
		return nil
	})
}

// CreateRun implements workflows.Store.
func (m *Mem) CreateRun(_ context.Context, r workflows.Run) (*workflows.Run, error) {
	if r.ProjectID == "" || r.WorkflowID == "" {
		return nil, fmt.Errorf("%w: a run needs a project and a workflow", workflows.ErrInvalid)
	}
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	now := m.stamp()
	r.Version, r.CreatedAt, r.UpdatedAt = 1, now, now
	err := m.mutate(func(st *state) error {
		if _, ok := st.Runs[r.ID]; ok {
			return workflows.ErrConflict
		}
		st.Runs[r.ID] = copyOf(r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// GetRun implements workflows.Store.
func (m *Mem) GetRun(_ context.Context, id string) (*workflows.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.st.Runs[id]
	if !ok {
		return nil, workflows.ErrNotFound
	}
	r = copyOf(r)
	return &r, nil
}

func matches(r workflows.Run, f workflows.RunFilter) bool {
	if f.ProjectID != "" && r.ProjectID != f.ProjectID || f.WorkflowID != "" && r.WorkflowID != f.WorkflowID ||
		f.ParentRunID != "" && r.ParentRunID != f.ParentRunID {
		return false
	}
	if len(f.States) == 0 {
		return true
	}
	for _, s := range f.States {
		if s == r.State {
			return true
		}
	}
	return false
}

// ListRuns implements workflows.Store.
func (m *Mem) ListRuns(_ context.Context, f workflows.RunFilter) ([]workflows.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []workflows.Run{}
	for _, r := range m.st.Runs {
		if matches(r, f) {
			out = append(out, copyOf(r))
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if !out[a].CreatedAt.Equal(out[b].CreatedAt) {
			return out[a].CreatedAt.After(out[b].CreatedAt)
		}
		return out[a].ID > out[b].ID
	})
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// UpdateRun implements workflows.Store.
func (m *Mem) UpdateRun(_ context.Context, r workflows.Run) (*workflows.Run, error) {
	now := m.stamp()
	var out workflows.Run
	err := m.mutate(func(st *state) error {
		cur, ok := st.Runs[r.ID]
		if !ok {
			return workflows.ErrNotFound
		}
		if r.Version != cur.Version {
			return workflows.ErrStale
		}
		r.ProjectID, r.WorkflowID, r.CreatedAt = cur.ProjectID, cur.WorkflowID, cur.CreatedAt
		r.Version, r.UpdatedAt = cur.Version+1, now
		st.Runs[r.ID] = copyOf(r)
		out = copyOf(r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ---- file ----

// fileVersion is the schema of the persisted file; Open refuses any other.
const fileVersion = 1

// File is a Mem persisted to a JSON file: every change rewrites the file atomically before it is applied, so
// memory never gets ahead of what a restart would restore. One process per file.
type File struct {
	*Mem
	path string
}

var _ workflows.Store = (*File)(nil)

type fileData struct {
	Version int   `json:"version"`
	State   state `json:"state"`
}

// Open returns a store persisted to path, loading the file if it exists. A missing file is an empty store; a file
// that cannot be read or parsed is an error naming the path and is left untouched.
func Open(path string, now func() time.Time) (*File, error) {
	f := &File{Mem: NewMem(now), path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("workflowstore: create directory of %s: %w", path, err)
	}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("workflowstore: read %s: %w", path, err)
	default:
		var d fileData
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, fmt.Errorf("workflowstore: load %s: %w", path, err)
		}
		if d.Version != fileVersion {
			return nil, fmt.Errorf("workflowstore: load %s: unsupported version %d (want %d)", path, d.Version, fileVersion)
		}
		f.st = d.State.normalized()
	}
	f.Mem.save = f.write
	return f, nil
}

// write replaces the file atomically: a 0600 temp file in the same directory, fsynced, renamed over the target,
// then the directory fsynced.
func (f *File) write(st state) error {
	b, err := json.Marshal(fileData{Version: fileVersion, State: st})
	if err != nil {
		return fmt.Errorf("workflowstore: encode %s: %w", f.path, err)
	}
	tmp := f.path + ".tmp"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("workflowstore: remove stale %s: %w", tmp, err)
	}
	t, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("workflowstore: write %s: %w", tmp, err)
	}
	if _, err = t.Write(b); err == nil {
		err = t.Sync()
	}
	if cerr := t.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("workflowstore: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, f.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("workflowstore: replace %s: %w", f.path, err)
	}
	if err := syncDir(filepath.Dir(f.path)); err != nil {
		return fmt.Errorf("workflowstore: sync directory of %s: %w", f.path, err)
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if errors.Is(err, syscall.EINVAL) {
		return nil
	}
	return err
}
