// Package factorystore implements factory.Store: Mem (in memory, for tests and
// single-process hosts) and PG (Postgres, shared by several processes).
package factorystore

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/factory"
)

const (
	maxFingerprints = 10
	defaultLimit    = 200
	maxDeliveries   = 4096
)

// Mem is a goroutine-safe in-memory factory.Store, optionally persisted to a file (see Open).
type Mem struct {
	mu         sync.Mutex
	now        func() time.Time
	tasks      map[string]*factory.Task
	events     map[string][]factory.TaskEvent
	nextEvent  int64
	triggers   map[string]*factory.Trigger
	deliveries map[string]struct{}
	deliveryQ  []string
	threads    map[string]factory.ThreadInfo
	// spend history for SpendSince: one entry per AddSpend.
	ledger []ledgerEntry

	// Persistence (Open): path "" never writes. dirty means coalesced changes (events, deliveries) not
	// yet in the file; timer is the pending trailing flush; writes counts completed file replacements.
	path       string
	flushEvery time.Duration
	dirty      bool
	closed     bool
	timer      *time.Timer
	writes     atomic.Int64
	failures   atomic.Int64 // failed trailing flushes
}

type ledgerEntry struct {
	Project string        `json:"project"`
	At      time.Time     `json:"at"`
	D       factory.Spend `json:"d"`
}

// NewMem returns an empty store. now (nil: time.Now) is the clock.
func NewMem(now func() time.Time) *Mem {
	if now == nil {
		now = time.Now
	}
	return &Mem{now: now, tasks: map[string]*factory.Task{}, events: map[string][]factory.TaskEvent{},
		triggers: map[string]*factory.Trigger{}, deliveries: map[string]struct{}{}, threads: map[string]factory.ThreadInfo{},
		flushEvery: flushEvery}
}

func cloneTask(t *factory.Task) *factory.Task {
	c := *t
	c.DependsOn = slices.Clone(t.DependsOn)
	c.Labels = slices.Clone(t.Labels)
	c.Fingerprints = slices.Clone(t.Fingerprints)
	return &c
}

func cloneTrigger(t *factory.Trigger) *factory.Trigger {
	c := *t
	if t.Match != nil {
		c.Match = make(map[string]string, len(t.Match))
		for k, v := range t.Match {
			c.Match[k] = v
		}
	}
	c.Template.Labels = slices.Clone(t.Template.Labels)
	return &c
}

// CreateTask implements factory.Store.
func (m *Mem) CreateTask(_ context.Context, t factory.Task) (*factory.Task, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.Source.Key != "" {
		for _, x := range m.tasks {
			if x.ProjectID == t.ProjectID && x.Source.Key == t.Source.Key {
				return cloneTask(x), false, nil
			}
		}
	}
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	if _, dup := m.tasks[t.ID]; dup {
		return nil, false, fmt.Errorf("%w: task %s exists", factory.ErrInvalid, t.ID)
	}
	for _, d := range t.DependsOn {
		if d == t.ID {
			return nil, false, fmt.Errorf("%w: task depends on itself", factory.ErrInvalid)
		}
		if _, ok := m.tasks[d]; !ok {
			return nil, false, fmt.Errorf("%w: dependency %s: %w", factory.ErrInvalid, d, factory.ErrNotFound)
		}
	}
	now := m.now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	t.State = factory.StateReady
	if m.unmet(t.DependsOn) {
		t.State = factory.StatePending
	}
	c := cloneTask(&t)
	m.tasks[c.ID] = c
	if err := m.saveLocked(); err != nil {
		delete(m.tasks, c.ID)
		return nil, false, err
	}
	return cloneTask(c), true, nil
}

// unmet reports whether some dependency is not done.
func (m *Mem) unmet(deps []string) bool {
	for _, d := range deps {
		if x := m.tasks[d]; x == nil || x.State != factory.StateDone {
			return true
		}
	}
	return false
}

// GetTask implements factory.Store.
func (m *Mem) GetTask(_ context.Context, id string) (*factory.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks[id]
	if t == nil {
		return nil, factory.ErrNotFound
	}
	return cloneTask(t), nil
}

// UpdateTask implements factory.Store.
func (m *Mem) UpdateTask(_ context.Context, id string, p factory.Patch) (*factory.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks[id]
	if t == nil {
		return nil, factory.ErrNotFound
	}
	if len(p.IfState) > 0 && !slices.Contains(p.IfState, t.State) {
		return nil, factory.ErrConflict
	}
	if p.State != nil && !p.State.Valid() {
		return nil, fmt.Errorf("%w: state %q", factory.ErrInvalid, *p.State)
	}
	if p.DependsOn != nil {
		for _, d := range *p.DependsOn {
			if d == id {
				return nil, fmt.Errorf("%w: task depends on itself", factory.ErrInvalid)
			}
			if _, ok := m.tasks[d]; !ok {
				return nil, fmt.Errorf("%w: dependency %s: %w", factory.ErrInvalid, d, factory.ErrNotFound)
			}
		}
	}
	prev := cloneTask(t)
	applyPatch(t, p, m.now())
	released := m.releaseDependents()
	if err := m.saveLocked(); err != nil {
		for _, r := range released { // before t: a released entry may be t itself, post-patch
			*m.tasks[r.ID] = r
		}
		*t = *prev
		return nil, err
	}
	return cloneTask(t), nil
}

// releaseDependents moves Pending tasks whose dependencies are all done to Ready and returns
// their previous values, so that a failed write can undo it.
func (m *Mem) releaseDependents() []factory.Task {
	var before []factory.Task
	for _, t := range m.tasks {
		if t.State == factory.StatePending && !m.unmet(t.DependsOn) {
			before = append(before, *cloneTask(t))
			t.State = factory.StateReady
			t.UpdatedAt = m.now()
		}
	}
	return before
}

// ListTasks implements factory.Store: newest priority first, then oldest first.
func (m *Mem) ListTasks(_ context.Context, f factory.Filter) ([]factory.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []factory.Task
	for _, t := range m.tasks {
		if f.ProjectID != "" && t.ProjectID != f.ProjectID || f.Owner != "" && t.Owner != f.Owner ||
			f.ParentID != "" && t.ParentID != f.ParentID || f.ThreadID != "" && t.ThreadID != f.ThreadID ||
			f.SourceKey != "" && t.Source.Key != f.SourceKey ||
			len(f.States) > 0 && !slices.Contains(f.States, t.State) {
			continue
		}
		out = append(out, *cloneTask(t))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	limit := f.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// AddSpend implements factory.Store.
func (m *Mem) AddSpend(_ context.Context, id string, d factory.Spend) (*factory.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks[id]
	if t == nil {
		return nil, factory.ErrNotFound
	}
	prev, ledgerLen := cloneTask(t), len(m.ledger)
	t.Spent.USD += d.USD
	t.Spent.Tokens += d.Tokens
	t.Spent.Runs += d.Runs
	t.Spent.Unpriced += d.Unpriced
	t.UpdatedAt = m.now()
	m.ledger = append(m.ledger, ledgerEntry{Project: t.ProjectID, At: m.now(), D: d})
	if err := m.saveLocked(); err != nil {
		*t = *prev
		m.ledger = m.ledger[:ledgerLen]
		return nil, err
	}
	return cloneTask(t), nil
}

// SpendSince implements factory.Store.
func (m *Mem) SpendSince(_ context.Context, projectID string, since time.Time) (factory.Spend, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var s factory.Spend
	for _, e := range m.ledger {
		if (projectID == "" || e.Project == projectID) && !e.At.Before(since) {
			s.USD += e.D.USD
			s.Tokens += e.D.Tokens
			s.Runs += e.D.Runs
			s.Unpriced += e.D.Unpriced
		}
	}
	return s, nil
}

// AppendEvent implements factory.Store. A task keeps its newest maxEventsPerTask events. The write
// to the file is coalesced and best-effort: see Open.
func (m *Mem) AppendEvent(_ context.Context, e factory.TaskEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tasks[e.TaskID] == nil {
		return factory.ErrNotFound
	}
	m.nextEvent++
	e.ID = m.nextEvent
	if e.At.IsZero() {
		e.At = m.now()
	}
	ev := append(m.events[e.TaskID], e)
	if len(ev) > maxEventsPerTask {
		ev = append(ev[:0], ev[len(ev)-maxEventsPerTask:]...)
	}
	m.events[e.TaskID] = ev
	m.scheduleLocked()
	return nil
}

// ListEvents implements factory.Store: oldest first; with a limit, the newest limit events.
func (m *Mem) ListEvents(_ context.Context, taskID string, limit int) ([]factory.TaskEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ev := m.events[taskID]
	if limit > 0 && len(ev) > limit {
		ev = ev[len(ev)-limit:]
	}
	return slices.Clone(ev), nil
}

// PutTrigger implements factory.Store (insert or replace by ID).
func (m *Mem) PutTrigger(_ context.Context, t factory.Trigger) (*factory.Trigger, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	if t.Kind != factory.TriggerCron && t.Kind != factory.TriggerWebhook {
		return nil, fmt.Errorf("%w: trigger kind %q", factory.ErrInvalid, t.Kind)
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = m.now()
	}
	c := cloneTrigger(&t)
	prev, had := m.triggers[c.ID]
	m.triggers[c.ID] = c
	if err := m.saveLocked(); err != nil {
		if had {
			m.triggers[c.ID] = prev
		} else {
			delete(m.triggers, c.ID)
		}
		return nil, err
	}
	return cloneTrigger(c), nil
}

// GetTrigger implements factory.Store.
func (m *Mem) GetTrigger(_ context.Context, id string) (*factory.Trigger, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.triggers[id]
	if t == nil {
		return nil, factory.ErrNotFound
	}
	return cloneTrigger(t), nil
}

// ListTriggers implements factory.Store ("" project: all), by creation time.
func (m *Mem) ListTriggers(_ context.Context, projectID string) ([]factory.Trigger, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []factory.Trigger
	for _, t := range m.triggers {
		if projectID == "" || t.ProjectID == projectID {
			out = append(out, *cloneTrigger(t))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// DeleteTrigger implements factory.Store.
func (m *Mem) DeleteTrigger(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.triggers[id] == nil {
		return factory.ErrNotFound
	}
	prev := m.triggers[id]
	delete(m.triggers, id)
	if err := m.saveLocked(); err != nil {
		m.triggers[id] = prev
		return err
	}
	return nil
}

// ClaimDueTriggers implements factory.Store. A cron trigger whose NextFireAt is
// zero is never due: the scheduler sets it when the trigger is stored.
func (m *Mem) ClaimDueTriggers(_ context.Context, now time.Time, next func(factory.Trigger) time.Time) ([]factory.Trigger, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var due []factory.Trigger
	for _, t := range m.triggers {
		if t.Kind != factory.TriggerCron || !t.Enabled || t.NextFireAt.IsZero() || t.NextFireAt.After(now) {
			continue
		}
		claimed := *cloneTrigger(t)
		t.LastFiredAt = t.NextFireAt
		t.NextFireAt = next(*t)
		due = append(due, claimed)
	}
	sort.Slice(due, func(i, j int) bool { return due[i].ID < due[j].ID })
	if len(due) > 0 {
		m.bestEffortLocked()
	}
	return due, nil
}

// SeenDelivery implements factory.Store; it remembers the last maxDeliveries ids.
func (m *Mem) SeenDelivery(_ context.Context, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.deliveries[id]; ok {
		return true, nil
	}
	m.deliveries[id] = struct{}{}
	m.deliveryQ = append(m.deliveryQ, id)
	if len(m.deliveryQ) > maxDeliveries {
		delete(m.deliveries, m.deliveryQ[0])
		m.deliveryQ = m.deliveryQ[1:]
	}
	m.scheduleLocked()
	return false, nil
}

// SetThreadInfo implements factory.Store.
func (m *Mem) SetThreadInfo(_ context.Context, ti factory.ThreadInfo) error {
	if ti.ThreadID == "" {
		return fmt.Errorf("%w: thread id", factory.ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	prev, had := m.threads[ti.ThreadID]
	m.threads[ti.ThreadID] = ti
	if err := m.saveLocked(); err != nil {
		if had {
			m.threads[ti.ThreadID] = prev
		} else {
			delete(m.threads, ti.ThreadID)
		}
		return err
	}
	return nil
}

// GetThreadInfo implements factory.Store.
func (m *Mem) GetThreadInfo(_ context.Context, threadID string) (*factory.ThreadInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ti, ok := m.threads[threadID]
	if !ok {
		return nil, factory.ErrNotFound
	}
	return &ti, nil
}

var _ factory.Store = (*Mem)(nil)
