// Package schedstore implements schedule.Store: Mem (in memory), File (Mem persisted to a JSON file) and PG (tables of
// migration 0580, applied by pgstore).
package schedstore

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/schedule"
)

// maxRunsPerSchedule bounds the history Mem keeps for a schedule: older runs are dropped when a new one starts.
const maxRunsPerSchedule = 500

// LostError is the error of a run that outlived its deadline without a process finishing it.
const LostError = "the run was lost: the process that executed it stopped"

// Mem is a schedule.Store in memory.
type Mem struct {
	mu        sync.Mutex
	schedules map[string]*schedule.Schedule
	runs      map[string][]schedule.Run // by schedule id, oldest first
	// persist is called with the state after every change (File sets it).
	persist func(state) error
}

type state struct {
	Schedules []schedule.Schedule `json:"schedules"`
	Runs      []schedule.Run      `json:"runs"`
}

var _ schedule.Store = (*Mem)(nil)

// NewMem returns an empty store.
func NewMem() *Mem {
	return &Mem{schedules: map[string]*schedule.Schedule{}, runs: map[string][]schedule.Run{}}
}

func (m *Mem) snapshot() state {
	var st state
	for _, s := range m.schedules {
		st.Schedules = append(st.Schedules, *s)
	}
	sort.Slice(st.Schedules, func(i, j int) bool { return st.Schedules[i].ID < st.Schedules[j].ID })
	for _, rs := range m.runs {
		st.Runs = append(st.Runs, rs...)
	}
	sort.Slice(st.Runs, func(i, j int) bool {
		if !st.Runs[i].StartedAt.Equal(st.Runs[j].StartedAt) {
			return st.Runs[i].StartedAt.Before(st.Runs[j].StartedAt)
		}
		return st.Runs[i].ID < st.Runs[j].ID
	})
	return st
}

// save persists after a change; the caller holds mu.
func (m *Mem) save() error {
	if m.persist == nil {
		return nil
	}
	return m.persist(m.snapshot())
}

func utc(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC()
}

// Create implements schedule.Store.
func (m *Mem) Create(_ context.Context, s schedule.Schedule) (*schedule.Schedule, error) {
	if err := s.Normalize(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.ID == "" {
		s.ID = uuid.NewString()
	}
	if m.schedules[s.ID] != nil {
		return nil, schedule.ErrInvalid
	}
	now := time.Now().UTC()
	s.CreatedAt, s.UpdatedAt = now, now
	s.NextRunAt, s.LastRunAt = utc(s.NextRunAt), time.Time{}
	s.LastStatus, s.ThreadID = "", ""
	m.schedules[s.ID] = &s
	out := s
	return &out, m.save()
}

// Get implements schedule.Store.
func (m *Mem) Get(_ context.Context, id string) (*schedule.Schedule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.schedules[id]
	if s == nil {
		return nil, schedule.ErrNotFound
	}
	out := *s
	return &out, nil
}

// Update implements schedule.Store.
func (m *Mem) Update(_ context.Context, s schedule.Schedule) (*schedule.Schedule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.schedules[s.ID]
	if cur == nil {
		return nil, schedule.ErrNotFound
	}
	s.Owner, s.ProjectID = cur.Owner, cur.ProjectID
	if err := s.Normalize(); err != nil {
		return nil, err
	}
	s.CreatedAt, s.LastRunAt, s.LastStatus, s.ThreadID = cur.CreatedAt, cur.LastRunAt, cur.LastStatus, cur.ThreadID
	if !s.ReuseSession {
		s.ThreadID = ""
	}
	s.UpdatedAt = time.Now().UTC()
	s.NextRunAt = utc(s.NextRunAt)
	*cur = s
	out := s
	return &out, m.save()
}

// Delete implements schedule.Store.
func (m *Mem) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.schedules[id] == nil {
		return schedule.ErrNotFound
	}
	delete(m.schedules, id)
	delete(m.runs, id)
	return m.save()
}

// List implements schedule.Store.
func (m *Mem) List(_ context.Context, projectID, owner string) ([]schedule.Schedule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []schedule.Schedule{}
	for _, s := range m.schedules {
		if s.ProjectID == projectID && (projectID != "" || s.Owner == owner) {
			out = append(out, *s)
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

func (m *Mem) running(scheduleID string) bool {
	for _, r := range m.runs[scheduleID] {
		if r.Status == schedule.StatusRunning {
			return true
		}
	}
	return false
}

func (m *Mem) addRun(r schedule.Run) {
	rs := append(m.runs[r.ScheduleID], r)
	if len(rs) > maxRunsPerSchedule {
		rs = rs[len(rs)-maxRunsPerSchedule:]
	}
	m.runs[r.ScheduleID] = rs
}

func newRun(s schedule.Schedule, trigger string, fireAt, now time.Time, by string) schedule.Run {
	return schedule.Run{
		ID: uuid.NewString(), ScheduleID: s.ID, FireAt: fireAt.UTC(), Trigger: trigger, Status: schedule.StatusRunning,
		StartedBy: by, StartedAt: now.UTC(), Deadline: now.UTC().Add(s.Timeout() + schedule.DeadlineGrace),
	}
}

// ClaimDue implements schedule.Store.
func (m *Mem) ClaimDue(_ context.Context, now time.Time, next func(schedule.Schedule, time.Time) time.Time) ([]schedule.Claim, error) {
	now = now.UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := false
	for id, rs := range m.runs { // a run whose process vanished must not block its schedule forever
		for i := range rs {
			if rs[i].Status == schedule.StatusRunning && rs[i].Deadline.Before(now) {
				rs[i].Status, rs[i].Error, rs[i].FinishedAt = schedule.StatusFailed, LostError, now
				if s := m.schedules[id]; s != nil {
					s.LastStatus = schedule.StatusFailed
				}
				changed = true
			}
		}
	}
	var ids []string
	for id, s := range m.schedules {
		if s.Enabled && !s.NextRunAt.IsZero() && !s.NextRunAt.After(now) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	var claims []schedule.Claim
	for _, id := range ids {
		s := m.schedules[id]
		fireAt := s.NextRunAt
		s.LastRunAt = now
		s.NextRunAt = utc(next(*s, now))
		changed = true
		if m.running(id) {
			skipped := newRun(*s, schedule.TriggerSchedule, fireAt, now, s.RunAs)
			skipped.Status, skipped.FinishedAt = schedule.StatusSkipped, now
			skipped.Error = "the previous run was still in progress"
			m.addRun(skipped)
			continue
		}
		r := newRun(*s, schedule.TriggerSchedule, fireAt, now, s.RunAs)
		m.addRun(r)
		s.LastStatus = schedule.StatusRunning
		claims = append(claims, schedule.Claim{Schedule: *s, Run: r})
	}
	if changed {
		if err := m.save(); err != nil {
			return nil, err
		}
	}
	return claims, nil
}

// StartManual implements schedule.Store.
func (m *Mem) StartManual(_ context.Context, id, by string, now time.Time) (*schedule.Claim, error) {
	now = now.UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.schedules[id]
	if s == nil {
		return nil, schedule.ErrNotFound
	}
	for i, r := range m.runs[id] { // the same lost-run rule as ClaimDue, so a crashed run does not block run-now either
		if r.Status == schedule.StatusRunning && r.Deadline.Before(now) {
			m.runs[id][i].Status, m.runs[id][i].Error, m.runs[id][i].FinishedAt = schedule.StatusFailed, LostError, now
		}
	}
	if m.running(id) {
		return nil, schedule.ErrBusy
	}
	r := newRun(*s, schedule.TriggerManual, now, now, by)
	m.addRun(r)
	s.LastRunAt, s.LastStatus = now, schedule.StatusRunning
	return &schedule.Claim{Schedule: *s, Run: r}, m.save()
}

// FinishRun implements schedule.Store.
func (m *Mem) FinishRun(_ context.Context, runID string, f schedule.Finish, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, rs := range m.runs {
		for i := range rs {
			if rs[i].ID != runID {
				continue
			}
			r := &rs[i]
			if r.Status != schedule.StatusRunning {
				return nil // finished already (a lost run that came back): the first outcome stands
			}
			r.Status, r.Error, r.FinishedAt, r.Usage = f.Status, f.Error, now.UTC(), f.Usage
			if f.ThreadID != "" {
				r.ThreadID = f.ThreadID
			}
			if f.AgentRun != "" {
				r.AgentRun = f.AgentRun
			}
			if s := m.schedules[id]; s != nil {
				s.LastStatus = f.Status
				if r.ThreadID != "" && s.ReuseSession {
					s.ThreadID = r.ThreadID
				}
			}
			return m.save()
		}
	}
	return schedule.ErrNotFound
}

// SetThread implements schedule.Store.
func (m *Mem) SetThread(_ context.Context, runID, threadID, agentRunID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, rs := range m.runs {
		for i := range rs {
			if rs[i].ID != runID {
				continue
			}
			rs[i].ThreadID = threadID
			if agentRunID != "" {
				rs[i].AgentRun = agentRunID
			}
			if s := m.schedules[id]; s != nil && s.ReuseSession {
				s.ThreadID = threadID
			}
			return m.save()
		}
	}
	return schedule.ErrNotFound
}

// ClearThread implements schedule.Store.
func (m *Mem) ClearThread(_ context.Context, threadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := false
	for _, s := range m.schedules {
		if s.ThreadID == threadID {
			s.ThreadID, changed = "", true
		}
	}
	if !changed {
		return nil
	}
	return m.save()
}

// Runs implements schedule.Store.
func (m *Mem) Runs(_ context.Context, scheduleID string, limit int) ([]schedule.Run, error) {
	if limit <= 0 {
		limit = 50
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.schedules[scheduleID] == nil {
		return nil, schedule.ErrNotFound
	}
	rs := append([]schedule.Run(nil), m.runs[scheduleID]...)
	sort.SliceStable(rs, func(i, j int) bool {
		if !rs[i].StartedAt.Equal(rs[j].StartedAt) {
			return rs[i].StartedAt.After(rs[j].StartedAt)
		}
		return rs[i].ID > rs[j].ID
	})
	if len(rs) > limit {
		rs = rs[:limit]
	}
	return rs, nil
}

// RunOfThread implements schedule.Store.
func (m *Mem) RunOfThread(_ context.Context, threadID string) (*schedule.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *schedule.Run
	for _, rs := range m.runs {
		for i := range rs {
			if rs[i].ThreadID == threadID && (best == nil || rs[i].StartedAt.After(best.StartedAt)) {
				best = &rs[i]
			}
		}
	}
	if best == nil {
		return nil, schedule.ErrNotFound
	}
	out := *best
	return &out, nil
}
