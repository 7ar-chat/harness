// Package memstore is an in-memory store.Store used by tests (and nothing else).
package memstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/store"
)

// Store is a goroutine-safe in-memory implementation of store.Store.
type Store struct {
	mu       sync.Mutex
	threads  map[string]*thread
	runs     map[string]*store.Run
	messages []*store.Message
	events   map[string][]store.Event
	pos      int64
	h        harness
	// approvals by id (approvals.go).
	approvals map[string]*store.Approval
	mem       memory
	// Now is the clock; tests may override it.
	Now func() time.Time

	extrasOnce sync.Once
	ext        *extras // artifacts, blobs, skills (artifacts.go)

	ideaUsage []*store.IdeaUsage // ideas.go
	catOnce   sync.Once
	cat       *catalog                 // skill versions, global skills, sessions (skills_catalog.go)
	renders   map[string]*store.Render // by thread id + "\x00" + render id (renders.go)
}

type thread struct {
	store.Thread
	deleted   bool
	deletedAt time.Time
}

// ErrInvalidText mimics Postgres rejecting NUL in text (22021) and \u0000 in jsonb (22P05).
var ErrInvalidText = fmt.Errorf("memstore: invalid byte sequence (NUL or invalid UTF-8), rejected like Postgres: %w", store.ErrInvalidData)

func badText(ss ...string) bool {
	for _, s := range ss {
		if strings.ContainsRune(s, 0) || !utf8.ValidString(s) {
			return true
		}
	}
	return false
}

func badJSON(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	if !utf8.Valid(b) {
		return true
	}
	if !bytes.Contains(b, []byte(`\u0000`)) {
		return false
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		return true
	}
	return hasNUL(v)
}

func hasNUL(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.ContainsRune(x, 0)
	case map[string]any:
		for k, val := range x {
			if strings.ContainsRune(k, 0) || hasNUL(val) {
				return true
			}
		}
	case []any:
		for _, val := range x {
			if hasNUL(val) {
				return true
			}
		}
	}
	return false
}

func badMessage(m *store.Message) bool {
	if badText(m.ID, m.Role, m.Content, m.ToolCallID, m.Error) {
		return true
	}
	tc, _ := json.Marshal(m.ToolCalls)
	md, _ := json.Marshal(m.Metadata)
	return badJSON(tc) || badJSON(md)
}

// New returns an empty store.
func New() *Store {
	return &Store{
		threads: map[string]*thread{},
		runs:    map[string]*store.Run{},
		events:  map[string][]store.Event{},
		Now:     time.Now,
	}
}

var _ store.Store = (*Store)(nil)

func (s *Store) Ping(context.Context) error { return nil }

func (s *Store) now() time.Time { return s.Now().UTC() }

func (s *Store) activeRunLocked(threadID string) *store.Run {
	for _, r := range s.runs {
		if r.ThreadID == threadID && !store.IsTerminal(r.Status) {
			return r
		}
	}
	return nil
}

func (s *Store) viewLocked(t *thread) *store.Thread {
	cp := t.Thread
	if r := s.activeRunLocked(t.ID); r != nil {
		cp.ActiveRun = &store.ActiveRun{ID: r.ID, Status: r.Status}
	}
	s.harnessViewLocked(&cp)
	s.notifyViewLocked(&cp)
	return &cp
}

func (s *Store) CreateThread(_ context.Context, login, title string) (*store.Thread, error) {
	if badText(title) {
		return nil, ErrInvalidText
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	t := &thread{Thread: store.Thread{ID: uuid.NewString(), UserLogin: login, Title: title, CreatedAt: now, UpdatedAt: now, TitleSource: titleSourceOf(title),
		LastSeenAt: &now}}
	s.threads[t.ID] = t
	return s.viewLocked(t), nil
}

func (s *Store) ownedLocked(login, id string) (*thread, error) {
	t, ok := s.threads[id]
	if !ok || t.deleted || t.UserLogin != login {
		return nil, store.ErrNotFound
	}
	return t, nil
}

func (s *Store) GetThread(_ context.Context, login, id string) (*store.Thread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.ownedLocked(login, id)
	if err != nil {
		return nil, err
	}
	return s.viewLocked(t), nil
}

func less(a, b store.ThreadCursor) bool {
	if a.Pinned != b.Pinned {
		return !a.Pinned
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}

func cursorOf(t *store.Thread) store.ThreadCursor {
	return store.ThreadCursor{Pinned: t.PinnedAt != nil, CreatedAt: t.CreatedAt, ID: t.ID}
}

func (s *Store) ListThreads(ctx context.Context, login string, limit int, before *store.ThreadCursor) ([]*store.Thread, error) {
	return s.SearchThreads(ctx, login, "", limit, before)
}

func (s *Store) UpdateThread(_ context.Context, login, id string, patch store.ThreadPatch) (*store.Thread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.ownedLocked(login, id)
	if err != nil {
		return nil, err
	}
	if patch.Title != nil {
		if badText(*patch.Title) {
			return nil, ErrInvalidText
		}
		t.Title = *patch.Title
		t.TitleSource = store.TitleUser
		t.UpdatedAt = s.now()
	}
	if patch.Pinned != nil {
		if *patch.Pinned && t.PinnedAt == nil {
			now := s.now()
			t.PinnedAt = &now
		} else if !*patch.Pinned {
			t.PinnedAt = nil
		}
	}
	return s.viewLocked(t), nil
}

func (s *Store) DeleteThread(_ context.Context, login, id string) (*store.Thread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.ownedLocked(login, id)
	if err != nil {
		return nil, err
	}
	v := s.viewLocked(t)
	t.deleted, t.deletedAt = true, s.now()
	t.SandboxID = ""
	return v, nil
}

func (s *Store) CreateRun(_ context.Context, in store.NewRun) (*store.Run, *store.Message, error) {
	if badMessage(&in.Message) || badText(in.AutoTitle) {
		return nil, nil, ErrInvalidText
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.threads[in.ThreadID]
	if !ok || t.deleted {
		return nil, nil, store.ErrNotFound
	}
	if s.activeRunLocked(in.ThreadID) != nil {
		return nil, nil, store.ErrRunActive
	}
	for _, m := range s.messages {
		if in.Message.Role != "" && in.Message.ID != "" && m.ID == in.Message.ID && m.ThreadID == in.ThreadID {
			return nil, nil, store.ErrConflict
		}
	}
	apply, err := s.prepareRunLocked(t, in)
	if err != nil {
		return nil, nil, err
	}
	now := s.now()
	if t.Title == "" {
		t.Title = in.AutoTitle
	}
	t.UpdatedAt = now
	if in.InteractionMode != "" {
		t.InteractionMode = in.InteractionMode
	}
	mode := t.InteractionMode
	if mode == "" {
		mode = store.InteractionAutomatic
	}
	r := &store.Run{ID: in.RunID, ThreadID: in.ThreadID, UserLogin: in.UserLogin, Status: store.StatusQueued,
		Model: in.Model, Userinfo: in.Userinfo, CreatedAt: now, Kind: runKind(in.Kind), ReasoningEffort: in.ReasoningEffort,
		InteractionMode: mode}
	s.runs[r.ID] = r
	apply(r.ID)
	s.recordRunSkills(r.ID, in.Skills)
	rc := *r
	if in.Message.Role == "" {
		return &rc, nil, nil
	}
	msg := in.Message
	msg.ThreadID = in.ThreadID
	msg.RunID = r.ID
	s.insertLocked(&msg)
	mc := msg
	return &rc, &mc, nil
}

func (s *Store) insertLocked(m *store.Message) {
	if m.ID == "" {
		m.ID = uuid.NewString()
	}
	s.pos++
	m.Position = s.pos
	m.CreatedAt = s.now()
	cp := *m
	s.messages = append(s.messages, &cp)
}

func (s *Store) GetRun(_ context.Context, id string) (*store.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (s *Store) GetUserRun(_ context.Context, login, id string) (*store.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok || r.UserLogin != login {
		return nil, store.ErrNotFound
	}
	if t := s.threads[r.ThreadID]; t == nil || t.deleted {
		return nil, store.ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (s *Store) ClaimRun(_ context.Context, id, runtimeID string) (*store.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok || r.Status != store.StatusQueued {
		return nil, store.ErrNotClaimable
	}
	now := s.now()
	if t := s.threads[r.ThreadID]; t == nil || t.deleted {
		// A queued run of a deleted thread is cancelled, never executed (like PG).
		r.Status, r.CancelRequested, r.FinishedAt = store.StatusCancelled, true, &now
		return nil, store.ErrNotClaimable
	}
	r.Status, r.RuntimeID, r.StartedAt, r.HeartbeatAt = store.StatusRunning, runtimeID, &now, &now
	cp := *r
	return &cp, nil
}

func (s *Store) Heartbeat(_ context.Context, id, runtimeID string) (bool, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok || r.Status != store.StatusRunning || r.RuntimeID != runtimeID {
		return false, false, nil
	}
	now := s.now()
	r.HeartbeatAt = &now
	return true, r.CancelRequested, nil
}

func (s *Store) RequestCancel(_ context.Context, id string) (*store.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	r.CancelRequested = true
	cp := *r
	return &cp, nil
}

func (s *Store) CancelQueuedRun(_ context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok || r.Status != store.StatusQueued {
		return false, nil
	}
	now := s.now()
	r.Status, r.CancelRequested, r.FinishedAt = store.StatusCancelled, true, &now
	return true, nil
}

func (s *Store) FinishRun(_ context.Context, id, runtimeID, status, errCode, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	if !ok || r.Status != store.StatusRunning || r.RuntimeID != runtimeID {
		return store.ErrNotOwned
	}
	now := s.now()
	r.Status, r.Error, r.FinishedAt = status, errMsg, &now
	r.ErrorCode = store.ErrorCodeFor(status, errCode)
	return nil
}

func (s *Store) ListQueuedRunsOlderThan(_ context.Context, age time.Duration) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	cutoff := s.now().Add(-age)
	for _, r := range s.runs {
		if r.Status == store.StatusQueued && r.CreatedAt.Before(cutoff) {
			out = append(out, r.ID)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *Store) InterruptStaleRuns(_ context.Context, age time.Duration, alive ...func(*store.Run) bool) ([]*store.Run, error) {
	stale := func(r *store.Run, cutoff time.Time) bool {
		return r.Status == store.StatusRunning && r.HeartbeatAt != nil && r.HeartbeatAt.Before(cutoff)
	}
	// Like Postgres: the alive checks run without the lock, the update re-checks.
	s.mu.Lock()
	var candidates []*store.Run
	cutoff := s.now().Add(-age)
	for _, r := range s.runs {
		if stale(r, cutoff) {
			cp := *r
			candidates = append(candidates, &cp)
		}
	}
	s.mu.Unlock()
	var ids []string
	for _, r := range candidates {
		if !store.Vouched(r, alive) {
			ids = append(ids, r.ID)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*store.Run
	cutoff = s.now().Add(-age)
	for _, id := range ids {
		if r := s.runs[id]; r != nil && stale(r, cutoff) {
			now := s.now()
			r.Status, r.FinishedAt, r.ErrorCode = store.StatusInterrupted, &now, "interrupted"
			r.Error = "The run was interrupted because its runtime stopped responding."
			cp := *r
			out = append(out, &cp)
		}
	}
	return out, nil
}

// SetHeartbeat lets tests age a run's heartbeat.
func (s *Store) SetHeartbeat(id string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[id]; ok {
		r.HeartbeatAt = &at
	}
}

// SetRunCreatedAt lets tests age a queued run.
func (s *Store) SetRunCreatedAt(id string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[id]; ok {
		r.CreatedAt = at
	}
}

func (s *Store) ListMessages(_ context.Context, threadID string) ([]*store.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*store.Message
	for _, m := range s.messages {
		if m.ThreadID == threadID && !s.h.superseded[msgKey(m.ThreadID, m.ID)] {
			cp := *m
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (s *Store) AppendMessages(_ context.Context, msgs ...*store.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range msgs {
		if badMessage(m) {
			return ErrInvalidText
		}
		for _, existing := range s.messages {
			if m.ID != "" && existing.ID == m.ID && existing.ThreadID == m.ThreadID {
				return store.ErrConflict
			}
		}
	}
	for _, m := range msgs {
		s.insertLocked(m)
	}
	return nil
}

func (s *Store) AppendEvents(_ context.Context, runID string, events []store.Event) error {
	for _, e := range events {
		if badJSON(e.Data) {
			return ErrInvalidText
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing := s.events[runID]
	have := map[int64]bool{}
	for _, e := range existing {
		have[e.Seq] = true
	}
	for _, e := range events {
		if have[e.Seq] {
			continue
		}
		existing = append(existing, e)
		if r, ok := s.runs[runID]; ok && e.Seq > r.LastSeq {
			r.LastSeq = e.Seq
		}
	}
	sort.Slice(existing, func(i, j int) bool { return existing[i].Seq < existing[j].Seq })
	s.events[runID] = existing
	return nil
}

// EventLogs returns a copy of every run's persisted event log (test assertions
// over all runs, e.g. the AG-UI order check of the runtime rigs).
func (s *Store) EventLogs() map[string][]store.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]store.Event, len(s.events))
	for id, evs := range s.events {
		out[id] = append([]store.Event(nil), evs...)
	}
	return out
}

func (s *Store) ListEvents(_ context.Context, runID string, afterSeq int64, limit int) ([]store.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 1000
	}
	var out []store.Event
	for _, e := range s.events[runID] {
		if e.Seq > afterSeq {
			out = append(out, e)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (s *Store) ThreadSandbox(_ context.Context, threadID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.threads[threadID]
	if !ok || t.deleted {
		return "", store.ErrNotFound
	}
	return t.SandboxID, nil
}

func (s *Store) SetThreadSandbox(_ context.Context, threadID, expected, sandboxID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.threads[threadID]
	if ok && !t.deleted && t.SandboxID == expected {
		t.SandboxID = sandboxID
	}
	return nil
}

func (s *Store) CountUserSandboxes(_ context.Context, login string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, t := range s.threads {
		if t.UserLogin == login && !t.deleted && t.SandboxID != "" {
			n++
		}
	}
	return n, nil
}

func (s *Store) LastEvent(_ context.Context, runID string) (store.Event, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	evs := s.events[runID]
	if len(evs) == 0 {
		return store.Event{}, false, nil
	}
	return evs[len(evs)-1], true, nil
}

func (s *Store) SetRunOutcome(_ context.Context, id, status, errCode, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[id]; ok {
		r.Status, r.ErrorCode, r.Error = status, store.ErrorCodeFor(status, errCode), errMsg
		if r.FinishedAt == nil {
			now := s.now()
			r.FinishedAt = &now
		}
	}
	return nil
}
