package memstore

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/store"
)

// approvalsLocked returns the approval list (created lazily).
func (s *Store) approvalsLocked() map[string]*store.Approval {
	if s.approvals == nil {
		s.approvals = map[string]*store.Approval{}
	}
	return s.approvals
}

func copyApproval(a *store.Approval) *store.Approval {
	if a == nil {
		return nil
	}
	cp := *a
	if a.ResolvedAt != nil {
		t := *a.ResolvedAt
		cp.ResolvedAt = &t
	}
	return &cp
}

// pendingApprovalLocked is the approval the thread waits for: pending, of a completed run.
func (s *Store) pendingApprovalLocked(threadID string) *store.Approval {
	var out *store.Approval
	for _, a := range s.approvalsLocked() {
		if a.ThreadID != threadID || a.Status != store.ApprovalPending {
			continue
		}
		if r := s.runs[a.RunID]; r == nil || r.Status != store.StatusCompleted {
			continue
		}
		if out == nil || a.CreatedAt.After(out.CreatedAt) {
			out = a
		}
	}
	return out
}

func (s *Store) CreateApproval(_ context.Context, a *store.Approval) error {
	if badText(a.ToolCallID, a.ToolName, a.ToolInputPreview) {
		return ErrInvalidText
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.threads[a.ThreadID]; !ok {
		return store.ErrNotFound
	}
	if _, ok := s.runs[a.RunID]; !ok {
		return store.ErrNotFound
	}
	for _, o := range s.approvalsLocked() {
		if o.RunID == a.RunID && o.Status == store.ApprovalPending {
			return store.ErrConflict
		}
	}
	if a.ID == "" {
		a.ID = uuid.NewString()
	}
	if a.Kind == "" {
		a.Kind = store.ApprovalKindRiskyTool
	}
	a.Status = store.ApprovalPending
	a.CreatedAt = s.now()
	s.approvalsLocked()[a.ID] = copyApproval(a)
	return nil
}

func (s *Store) GetApproval(_ context.Context, id string) (*store.Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.approvalsLocked()[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return copyApproval(a), nil
}

func (s *Store) ResumedApproval(_ context.Context, runID string) (*store.Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out *store.Approval
	for _, a := range s.approvalsLocked() {
		if a.ResumeRunID == runID && (out == nil || out.ResolvedAt == nil || a.ResolvedAt != nil && a.ResolvedAt.After(*out.ResolvedAt)) {
			out = a
		}
	}
	return copyApproval(out), nil
}

func (s *Store) CancelRunApprovals(_ context.Context, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, a := range s.approvalsLocked() {
		if a.RunID == runID && a.Status == store.ApprovalPending {
			a.Status, a.ResolvedAt = store.ApprovalCancelled, &now
		}
	}
	return nil
}

func (s *Store) CancelPendingApproval(_ context.Context, runID, result string) (*store.Approval, error) {
	if badText(result) {
		return nil, ErrInvalidText
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[runID]
	if r == nil {
		return nil, store.ErrApprovalNotPending
	}
	a := s.pendingApprovalLocked(r.ThreadID)
	if a == nil || a.RunID != runID {
		return nil, store.ErrApprovalNotPending
	}
	now := s.now()
	a.Status, a.ResolvedAt = store.ApprovalCancelled, &now
	s.insertLocked(&store.Message{ThreadID: r.ThreadID, RunID: runID, Role: store.RoleTool, ToolCallID: a.ToolCallID, Content: result})
	return copyApproval(a), nil
}

func (s *Store) ReopenApproval(_ context.Context, id string) (*store.Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.approvalsLocked()[id]
	if a == nil || a.Status != store.ApprovalResolved {
		return nil, store.ErrApprovalNotPending
	}
	resume, paused := s.runs[a.ResumeRunID], s.runs[a.RunID]
	if resume == nil || paused == nil || paused.Status != store.StatusCompleted {
		return nil, store.ErrApprovalNotPending
	}
	switch resume.Status {
	case store.StatusCompleted, store.StatusFailed, store.StatusCancelled, store.StatusInterrupted:
	default:
		return nil, store.ErrApprovalNotPending
	}
	for _, o := range s.runs {
		if o.ThreadID == a.ThreadID && o.ID != resume.ID &&
			(o.Status == store.StatusQueued || o.Status == store.StatusRunning || o.CreatedAt.After(resume.CreatedAt)) {
			return nil, store.ErrApprovalNotPending
		}
	}
	for _, m := range s.messages {
		if m.ThreadID == a.ThreadID && m.Role == store.RoleTool && m.ToolCallID == a.ToolCallID && !s.h.superseded[msgKey(m.ThreadID, m.ID)] {
			return nil, store.ErrApprovalNotPending
		}
	}
	a.Status, a.Decision, a.Response, a.ResumeRunID, a.ResolvedAt = store.ApprovalPending, "", store.ApprovalResponse{}, "", nil
	return copyApproval(a), nil
}

// ApprovalsOf returns a thread's approvals, oldest first (tests).
func (s *Store) ApprovalsOf(threadID string) []*store.Approval {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*store.Approval
	for _, a := range s.approvalsLocked() {
		if a.ThreadID == threadID {
			out = append(out, copyApproval(a))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// prepareApprovalLocked validates a new run against the thread's pending
// approval (like Postgres) and returns the mutation to apply once the run
// exists and whether in.Resume decided the approval.
func (s *Store) prepareApprovalLocked(t *thread, in store.NewRun) (apply func(runID string) *store.Message, handled bool, err error) {
	a := s.pendingApprovalLocked(t.ID)
	if a == nil {
		if in.Kind == store.KindCompact {
			return nil, false, nil
		}
		// Approvals cancelled with their waiting run (stop) are announced by the next run.
		return func(runID string) *store.Message {
			for _, o := range s.approvalsLocked() {
				if r := s.runs[o.RunID]; o.ThreadID == t.ID && (o.Status == store.ApprovalCancelled || o.Status == store.ApprovalExpired) && o.ResumeRunID == "" &&
					r != nil && r.Status == store.StatusCompleted {
					o.ResumeRunID = runID
				}
			}
			return nil
		}, false, nil
	}
	var content, status, decision string
	scope := store.ApprovalScopeOnce
	resp := store.ApprovalResponse{}
	switch {
	case in.Resume != nil && in.Resume.InterruptID == a.ID:
		decision = in.Resume.Decision
		if decision == "" {
			decision = store.DecisionDeny
		}
		resp = in.Resume.Response
		if badText(resp.SelectedChoice, resp.ResponseText) {
			return nil, false, ErrInvalidText
		}
		handled, status, content = true, store.ApprovalResolved, store.ApprovalResult(decision, resp)
		if in.Resume.Scope == store.ApprovalScopeRun && decision == store.DecisionApprove {
			scope = store.ApprovalScopeRun
		}
	case in.Resume != nil:
		return nil, false, nil
	case in.Message.Role == "":
		return nil, false, store.ErrApprovalPending
	default:
		status, content = store.ApprovalCancelled, store.ImplicitApprovalCancelResult
	}
	return func(runID string) *store.Message {
		now := s.now()
		a.Status, a.Decision, a.Response, a.ResumeRunID, a.ResolvedAt = status, decision, resp, runID, &now
		if handled {
			a.Scope = scope
			if r, paused := s.runs[runID], s.runs[a.RunID]; r != nil && paused != nil {
				r.ApprovedTools = store.ApprovedToolsAfter(paused.ApprovedTools, a)
			}
		}
		if content == "" {
			return nil
		}
		m := &store.Message{ThreadID: t.ID, RunID: runID, Role: store.RoleTool, ToolCallID: a.ToolCallID, Content: content}
		s.insertLocked(m)
		return m
	}, handled, nil
}

func (s *Store) ExpireApprovals(_ context.Context, ttl time.Duration, threadID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	n := 0
	for _, a := range s.approvalsLocked() {
		if a.Status != store.ApprovalPending || !a.CreatedAt.Before(now.Add(-ttl)) || (threadID != "" && a.ThreadID != threadID) {
			continue
		}
		a.Status, a.ResolvedAt = store.ApprovalExpired, &now
		if r := s.runs[a.RunID]; r != nil && r.Status == store.StatusCompleted {
			s.insertLocked(&store.Message{ThreadID: a.ThreadID, RunID: a.RunID, Role: store.RoleTool, ToolCallID: a.ToolCallID, Content: store.ApprovalExpiredResult})
		}
		n++
	}
	return n, nil
}

func (s *Store) LatestExpiredApproval(_ context.Context, threadID string) (*store.Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest *store.Approval
	for _, a := range s.approvalsLocked() {
		if a.ThreadID == threadID && (latest == nil || a.CreatedAt.After(latest.CreatedAt) || (a.CreatedAt.Equal(latest.CreatedAt) && a.ID > latest.ID)) {
			latest = a
		}
	}
	var last *store.Run
	for _, r := range s.runs {
		if r.ThreadID == threadID && (last == nil || r.CreatedAt.After(last.CreatedAt) || (r.CreatedAt.Equal(last.CreatedAt) && r.ID > last.ID)) {
			last = r
		}
	}
	if latest == nil || latest.Status != store.ApprovalExpired || last == nil || latest.RunID != last.ID {
		return nil, nil
	}
	return copyApproval(latest), nil
}

func (s *Store) SetThreadInteractionMode(_ context.Context, login, threadID, mode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, err := s.ownedLocked(login, threadID)
	if err != nil {
		return err
	}
	t.InteractionMode = mode
	if r := s.activeRunLocked(threadID); r != nil {
		r.InteractionMode = mode
	}
	return nil
}

func (s *Store) RunInteractionMode(_ context.Context, runID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[runID]
	if !ok {
		return "", store.ErrNotFound
	}
	return r.InteractionMode, nil
}
