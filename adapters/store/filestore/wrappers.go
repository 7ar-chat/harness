package filestore

import (
	"context"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

// Every mutating store.Full method is applied to memstore first and the
// threads it touched are then journaled (see syncThreads). Methods not listed
// here are read-only, or concern state that is not persisted; anything they
// change in a persisted thread is journaled by the next wrapped call on it.

// after journals the given threads when err is nil.
func (s *Store) after(err error, threadIDs ...string) error {
	if err != nil {
		return err
	}
	return s.syncThreads(threadIDs...)
}

func (s *Store) CreateThread(ctx context.Context, login, title string) (*store.Thread, error) {
	t, err := s.Store.CreateThread(ctx, login, title)
	if err != nil {
		return nil, err
	}
	if err := s.syncThreads(t.ID); err != nil {
		return nil, err
	}
	return t, nil
}

func (s *Store) UpdateThread(ctx context.Context, login, id string, patch store.ThreadPatch) (*store.Thread, error) {
	t, err := s.Store.UpdateThread(ctx, login, id, patch)
	if err := s.after(err, id); err != nil {
		return nil, err
	}
	return t, nil
}

func (s *Store) DeleteThread(ctx context.Context, login, id string) (*store.Thread, error) {
	t, err := s.Store.DeleteThread(ctx, login, id)
	if err := s.after(err, id); err != nil {
		return nil, err
	}
	return t, nil
}

func (s *Store) MarkThreadSeen(ctx context.Context, login, id string) error {
	return s.after(s.Store.MarkThreadSeen(ctx, login, id), id)
}

func (s *Store) CreateRun(ctx context.Context, in store.NewRun) (*store.Run, *store.Message, error) {
	r, m, err := s.Store.CreateRun(ctx, in)
	if err := s.after(err, in.ThreadID); err != nil {
		return nil, nil, err
	}
	return r, m, nil
}

func (s *Store) ClaimRun(ctx context.Context, id, runtimeID string) (*store.Run, error) {
	r, err := s.Store.ClaimRun(ctx, id, runtimeID)
	if err != nil {
		// A queued run of a deleted thread is cancelled by the claim.
		_ = s.syncRun(id)
		return nil, err
	}
	if err := s.syncRun(id); err != nil {
		return nil, err
	}
	return r, nil
}

func (s *Store) RequestCancel(ctx context.Context, id string) (*store.Run, error) {
	r, err := s.Store.RequestCancel(ctx, id)
	if err := s.after(err); err != nil {
		return nil, err
	}
	return r, s.syncRun(id)
}

func (s *Store) CancelQueuedRun(ctx context.Context, id string) (bool, error) {
	ok, err := s.Store.CancelQueuedRun(ctx, id)
	if err != nil || !ok {
		return ok, err
	}
	return ok, s.syncRun(id)
}

func (s *Store) FinishRun(ctx context.Context, id, runtimeID, status, errCode, errMsg string) error {
	err := s.Store.FinishRun(ctx, id, runtimeID, status, errCode, errMsg)
	// A run end is when messages of side paths (artifact notes) that skipped
	// the wrappers are picked up.
	if err != nil {
		return err
	}
	return s.syncRunEnd(id)
}

func (s *Store) SetRunOutcome(ctx context.Context, id, status, errCode, errMsg string) error {
	if err := s.Store.SetRunOutcome(ctx, id, status, errCode, errMsg); err != nil {
		return err
	}
	return s.syncRun(id)
}

func (s *Store) InterruptStaleRuns(ctx context.Context, age time.Duration, alive ...func(*store.Run) bool) ([]*store.Run, error) {
	runs, err := s.Store.InterruptStaleRuns(ctx, age, alive...)
	var ids []string
	for _, r := range runs {
		ids = append(ids, r.ThreadID)
	}
	if err := s.after(err, ids...); err != nil {
		return nil, err
	}
	return runs, nil
}

func (s *Store) AppendMessages(ctx context.Context, msgs ...*store.Message) error {
	if err := s.Store.AppendMessages(ctx, msgs...); err != nil {
		return err
	}
	return s.journalMessages(msgs...)
}

func (s *Store) AppendEvents(ctx context.Context, runID string, events []store.Event) error {
	if err := s.Store.AppendEvents(ctx, runID, events); err != nil {
		return err
	}
	if err := s.journalEvents(runID, events); err != nil {
		return err
	}
	return s.syncRun(runID) // LastSeq
}

func (s *Store) AppendToolVerification(ctx context.Context, threadID, toolCallID string, items []any) error {
	if err := s.Store.AppendToolVerification(ctx, threadID, toolCallID, items); err != nil {
		return err
	}
	_, msgs, _ := s.Store.ExportThread(threadID)
	s.jmu.Lock()
	defer s.jmu.Unlock()
	b := &batch{s: s}
	for i := len(msgs) - 1; i >= 0; i-- {
		if m := msgs[i]; m.Role == store.RoleTool && m.ToolCallID == toolCallID {
			b.add("msg", msgKey(m.ThreadID, m.ID), m, false) // later record wins on replay
			break
		}
	}
	return b.flush()
}

func (s *Store) SetThreadSummary(ctx context.Context, threadID string, sum store.ThreadSummary) error {
	return s.after(s.Store.SetThreadSummary(ctx, threadID, sum), threadID)
}

func (s *Store) SetThreadPlan(ctx context.Context, threadID string, items []store.PlanItem) error {
	return s.after(s.Store.SetThreadPlan(ctx, threadID, items), threadID)
}

func (s *Store) SetPendingQuestion(ctx context.Context, threadID string, q *store.PendingQuestion) error {
	return s.after(s.Store.SetPendingQuestion(ctx, threadID, q), threadID)
}

func (s *Store) SetGeneratedTitle(ctx context.Context, threadID, title string) (bool, error) {
	ok, err := s.Store.SetGeneratedTitle(ctx, threadID, title)
	if err != nil || !ok {
		return ok, err
	}
	return ok, s.syncThreads(threadID)
}

func (s *Store) PutGoal(ctx context.Context, login, threadID, text string, maxTurns int) (*store.Goal, error) {
	g, err := s.Store.PutGoal(ctx, login, threadID, text, maxTurns)
	if err := s.after(err, threadID); err != nil {
		return nil, err
	}
	return g, nil
}

func (s *Store) ClearGoal(ctx context.Context, login, threadID string) error {
	return s.after(s.Store.ClearGoal(ctx, login, threadID), threadID)
}

func (s *Store) UpdateGoal(ctx context.Context, threadID string, u store.GoalUpdate) (*store.Goal, error) {
	g, err := s.Store.UpdateGoal(ctx, threadID, u)
	if err := s.after(err, threadID); err != nil {
		return nil, err
	}
	return g, nil
}

func (s *Store) SetFeedback(ctx context.Context, login, messageID string, f store.Feedback) error {
	if err := s.Store.SetFeedback(ctx, login, messageID, f); err != nil {
		return err
	}
	return s.syncAll() // the thread of a message id is not known here; rare call
}

func (s *Store) DeleteFeedback(ctx context.Context, login, messageID string) error {
	if err := s.Store.DeleteFeedback(ctx, login, messageID); err != nil {
		return err
	}
	return s.syncAll()
}

func (s *Store) RecordCompaction(ctx context.Context, c store.Compaction) error {
	return s.after(s.Store.RecordCompaction(ctx, c), c.ThreadID)
}

func (s *Store) CreateApproval(ctx context.Context, a *store.Approval) error {
	return s.after(s.Store.CreateApproval(ctx, a), a.ThreadID)
}

func (s *Store) CancelRunApprovals(ctx context.Context, runID string) error {
	return s.after(s.Store.CancelRunApprovals(ctx, runID), s.runThread(runID))
}

func (s *Store) CancelPendingApproval(ctx context.Context, runID, result string) (*store.Approval, error) {
	a, err := s.Store.CancelPendingApproval(ctx, runID, result)
	if err := s.after(err, s.runThread(runID)); err != nil {
		return nil, err
	}
	return a, nil
}

func (s *Store) ReopenApproval(ctx context.Context, id string) (*store.Approval, error) {
	a, err := s.Store.ReopenApproval(ctx, id)
	if err != nil {
		return nil, err
	}
	if a != nil {
		if err := s.syncThreads(a.ThreadID); err != nil {
			return nil, err
		}
	}
	return a, nil
}

func (s *Store) ExpireApprovals(ctx context.Context, ttl time.Duration, threadID string) (int, error) {
	n, err := s.Store.ExpireApprovals(ctx, ttl, threadID)
	if err != nil || n == 0 {
		return n, err
	}
	if threadID == "" {
		return n, s.syncAll()
	}
	return n, s.syncThreads(threadID)
}

func (s *Store) SetThreadInteractionMode(ctx context.Context, login, threadID, mode string) error {
	return s.after(s.Store.SetThreadInteractionMode(ctx, login, threadID, mode), threadID)
}

// SupersedeFrom implements store.HistoryStore.
func (s *Store) SupersedeFrom(ctx context.Context, login, threadID, messageID string) error {
	return s.after(s.Store.SupersedeFrom(ctx, login, threadID, messageID), threadID)
}

// ---- memory entries ----

// journalMemory writes the entries (a nil entry value is a tombstone).
func (s *Store) journalMemory(entries ...*store.MemoryEntry) error {
	s.jmu.Lock()
	defer s.jmu.Unlock()
	b := &batch{s: s}
	for _, e := range entries {
		b.add("mem", e.ID, e, true)
	}
	return b.flush()
}

func (s *Store) journalMemoryGone(ids ...string) error {
	s.jmu.Lock()
	defer s.jmu.Unlock()
	b := &batch{s: s}
	for _, id := range ids {
		delete(s.cache, "mem\x00"+id)
		b.add("mem", id, (*store.MemoryEntry)(nil), false)
	}
	return b.flush()
}

func (s *Store) CreateMemoryEntry(ctx context.Context, e *store.MemoryEntry, limits store.MemoryLimits) (*store.MemoryEntry, error) {
	out, err := s.Store.CreateMemoryEntry(ctx, e, limits)
	if err != nil {
		return nil, err
	}
	return out, s.journalMemory(out)
}

func (s *Store) UpdateMemoryEntry(ctx context.Context, e *store.MemoryEntry, limits store.MemoryLimits) (*store.MemoryEntry, error) {
	out, err := s.Store.UpdateMemoryEntry(ctx, e, limits)
	if err != nil {
		return nil, err
	}
	return out, s.journalMemory(out)
}

func (s *Store) DeleteMemoryEntry(ctx context.Context, login, id, expectStatus string) error {
	if err := s.Store.DeleteMemoryEntry(ctx, login, id, expectStatus); err != nil {
		return err
	}
	return s.journalMemoryGone(id)
}

func (s *Store) SetMemoryEnabled(ctx context.Context, login string, enabled bool) error {
	if err := s.Store.SetMemoryEnabled(ctx, login, enabled); err != nil {
		return err
	}
	s.jmu.Lock()
	defer s.jmu.Unlock()
	b := &batch{s: s}
	b.add("memoff", login, !enabled, true)
	return b.flush()
}

func (s *Store) PurgeDeletedMemoryEntries(ctx context.Context, before time.Time) (int, error) {
	was, _ := s.Store.ExportMemory()
	n, err := s.Store.PurgeDeletedMemoryEntries(ctx, before)
	if err != nil || n == 0 {
		return n, err
	}
	now, _ := s.Store.ExportMemory()
	alive := map[string]bool{}
	for _, e := range now {
		alive[e.ID] = true
	}
	var gone []string
	for _, e := range was {
		if !alive[e.ID] {
			gone = append(gone, e.ID)
		}
	}
	return n, s.journalMemoryGone(gone...)
}
