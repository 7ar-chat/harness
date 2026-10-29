package memstore

import (
	"context"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/core/store"
)

// harness is the in-memory state of the harness v2 features.
type harness struct {
	superseded  map[string]bool // msgKey
	summaries   map[string]store.ThreadSummary
	goals       map[string]*store.Goal               // by thread id
	feedback    map[string]map[string]store.Feedback // thread id → message id
	compactions []store.Compaction
}

func (h *harness) init() {
	if h.superseded == nil {
		h.superseded = map[string]bool{}
		h.summaries = map[string]store.ThreadSummary{}
		h.goals = map[string]*store.Goal{}
		h.feedback = map[string]map[string]store.Feedback{}
	}
}

func msgKey(threadID, id string) string { return threadID + "\x00" + id }

func titleSourceOf(title string) string {
	if strings.TrimSpace(title) == "" {
		return store.TitleAuto
	}
	return store.TitleUser
}

func runKind(kind string) string {
	if kind == "" {
		return store.KindChat
	}
	return kind
}

func copyGoal(g *store.Goal) *store.Goal {
	if g == nil {
		return nil
	}
	cp := *g
	if g.LastVerdict != nil {
		v := *g.LastVerdict
		cp.LastVerdict = &v
	}
	return &cp
}

func (s *Store) harnessViewLocked(t *store.Thread) {
	s.h.init()
	t.Plan = append([]store.PlanItem(nil), t.Plan...)
	if t.PendingQuestion != nil {
		q := *t.PendingQuestion
		q.Options = append([]store.QuestionOption(nil), q.Options...)
		t.PendingQuestion = &q
	}
	t.Goal = copyGoal(s.h.goals[t.ID])
	t.PendingApproval = copyApproval(s.pendingApprovalLocked(t.ID))
}

// prepareRunLocked validates the harness parts of CreateRun (like Postgres, in
// one transaction) and returns the mutation to apply once the run exists.
func (s *Store) prepareRunLocked(t *thread, in store.NewRun) (func(runID string), error) {
	s.h.init()
	var replaceFrom int64 = -1
	if in.ReplaceFromMessageID != "" {
		for _, m := range s.messages {
			if m.ThreadID == t.ID && m.ID == in.ReplaceFromMessageID && m.Role == store.RoleUser && !s.h.superseded[msgKey(m.ThreadID, m.ID)] {
				replaceFrom = m.Position
			}
		}
		if replaceFrom < 0 {
			return nil, store.ErrInvalidReplace
		}
	}
	applyApproval, decided, err := s.prepareApprovalLocked(t, in)
	if err != nil {
		return nil, err
	}
	if decided {
		in.Resume = nil // the resume decided the pending approval
	}
	var result *store.Message
	if p := t.PendingQuestion; !decided && (p != nil || in.Resume != nil) {
		switch {
		case p == nil:
			return nil, store.ErrNoPendingQuestion
		case in.Resume != nil:
			if in.Resume.InterruptID != p.InterruptID {
				return nil, store.ErrNoPendingQuestion
			}
			result = &store.Message{Role: store.RoleTool, ToolCallID: p.ToolCallID, Content: in.Resume.Content}
		case in.Message.Role == "":
			return nil, store.ErrQuestionPending
		default: // a new message (also an edit) implicitly cancels the question
			result = &store.Message{Role: store.RoleTool, ToolCallID: p.ToolCallID, Content: store.ImplicitCancelResult}
		}
		if result != nil && badMessage(result) {
			return nil, ErrInvalidText
		}
	}
	var goal *store.Goal
	if in.GoalID != "" {
		goal = s.h.goals[t.ID]
		if goal == nil || goal.ID != in.GoalID || goal.Status != store.GoalActive || goal.TurnsUsed >= goal.MaxTurns {
			return nil, store.ErrGoalInactive
		}
	}
	return func(runID string) {
		if replaceFrom >= 0 {
			for _, m := range s.messages {
				if m.ThreadID == t.ID && m.Position >= replaceFrom {
					s.h.superseded[msgKey(m.ThreadID, m.ID)] = true
				}
			}
			if sum, ok := s.h.summaries[t.ID]; ok && sum.ThroughPosition >= replaceFrom {
				delete(s.h.summaries, t.ID)
			}
		}
		if applyApproval != nil {
			if m := applyApproval(runID); m != nil && replaceFrom >= 0 && !s.visibleCallLocked(t.ID, m.ToolCallID) {
				s.h.superseded[msgKey(t.ID, m.ID)] = true
			}
		}
		if result != nil {
			result.ThreadID, result.RunID = t.ID, runID
			s.insertLocked(result)
			if p := t.PendingQuestion; in.Resume != nil && p != nil {
				// The answer continues the paused run's task: same approved tool kinds (scope run).
				if r, paused := s.runs[runID], s.runs[p.RunID]; r != nil && paused != nil {
					r.ApprovedTools = append([]string(nil), paused.ApprovedTools...)
				}
			}
			if replaceFrom >= 0 && !s.visibleCallLocked(t.ID, result.ToolCallID) {
				s.h.superseded[msgKey(t.ID, result.ID)] = true // hidden with the call it answers
			}
		}
		if replaceFrom >= 0 {
			var visible []*store.Message
			for _, m := range s.messages {
				if m.ThreadID == t.ID && !s.h.superseded[msgKey(m.ThreadID, m.ID)] {
					visible = append(visible, m)
				}
			}
			t.Plan = store.LatestPlan(visible)
		}
		if !decided {
			t.PendingQuestion = nil
		}
		if goal != nil {
			goal.TurnsUsed++
			goal.UpdatedAt = s.now()
		}
	}, nil
}

// visibleCallLocked reports a visible assistant message holding the tool call.
func (s *Store) visibleCallLocked(threadID, toolCallID string) bool {
	for _, m := range s.messages {
		if m.ThreadID != threadID || m.Role != store.RoleAssistant || s.h.superseded[msgKey(m.ThreadID, m.ID)] {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID == toolCallID {
				return true
			}
		}
	}
	return false
}

func (s *Store) SearchThreads(_ context.Context, login, search string, limit int, before *store.ThreadCursor) ([]*store.Thread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h.init()
	needle := strings.ToLower(search)
	var all []*store.Thread
	for _, t := range s.threads {
		if t.deleted || t.UserLogin != login {
			continue
		}
		v := s.viewLocked(t)
		if before != nil && !less(cursorOf(v), *before) {
			continue
		}
		if needle != "" && !s.matchesLocked(t, needle) {
			continue
		}
		all = append(all, v)
	}
	sort.Slice(all, func(i, j int) bool { return less(cursorOf(all[j]), cursorOf(all[i])) })
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func (s *Store) matchesLocked(t *thread, needle string) bool {
	if strings.Contains(strings.ToLower(t.Title), needle) {
		return true
	}
	for _, m := range s.messages {
		if m.ThreadID == t.ID && (m.Role == store.RoleUser || m.Role == store.RoleAssistant) &&
			!s.h.superseded[msgKey(m.ThreadID, m.ID)] && strings.Contains(strings.ToLower(m.Content), needle) {
			return true
		}
	}
	return false
}

func (s *Store) GetThreadSummary(_ context.Context, threadID string) (store.ThreadSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h.init()
	if _, ok := s.threads[threadID]; !ok {
		return store.ThreadSummary{}, store.ErrNotFound
	}
	return s.h.summaries[threadID], nil
}

func (s *Store) SetThreadSummary(_ context.Context, threadID string, sum store.ThreadSummary) error {
	if badText(sum.Text) {
		return ErrInvalidText
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h.init()
	if sum.Text == "" {
		delete(s.h.summaries, threadID)
	} else {
		s.h.summaries[threadID] = sum
	}
	return nil
}

func (s *Store) SetThreadPlan(_ context.Context, threadID string, items []store.PlanItem) error {
	for _, it := range items {
		if badText(it.ID, it.Title, it.Status) {
			return ErrInvalidText
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.threads[threadID]; ok {
		t.Plan = append([]store.PlanItem{}, items...)
	}
	return nil
}

func (s *Store) SetPendingQuestion(_ context.Context, threadID string, q *store.PendingQuestion) error {
	if q != nil {
		if badText(q.Question, q.InterruptID, q.ToolCallID) {
			return ErrInvalidText
		}
		for _, o := range q.Options {
			if badText(o.Label, o.Description) {
				return ErrInvalidText
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.threads[threadID]; ok {
		if q == nil {
			t.PendingQuestion = nil
		} else {
			cp := *q
			cp.Options = append([]store.QuestionOption(nil), q.Options...)
			t.PendingQuestion = &cp
		}
	}
	return nil
}

func (s *Store) SetGeneratedTitle(_ context.Context, threadID, title string) (bool, error) {
	if badText(title) {
		return false, ErrInvalidText
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.threads[threadID]
	if !ok || t.deleted || t.TitleSource != store.TitleAuto {
		return false, nil
	}
	t.Title, t.TitleSource = title, store.TitleGenerated
	return true, nil
}

func (s *Store) PutGoal(_ context.Context, login, threadID, text string, maxTurns int) (*store.Goal, error) {
	if badText(text) {
		return nil, ErrInvalidText
	}
	if maxTurns < 1 || maxTurns > 20 {
		return nil, store.ErrInvalidData // CHECK (max_turns BETWEEN 1 AND 20)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h.init()
	if _, err := s.ownedLocked(login, threadID); err != nil {
		return nil, err
	}
	now := s.now()
	g := &store.Goal{ID: uuid.NewString(), ThreadID: threadID, Text: text, Status: store.GoalActive, MaxTurns: maxTurns, CreatedAt: now, UpdatedAt: now}
	s.h.goals[threadID] = g
	return copyGoal(g), nil
}

func (s *Store) GetGoal(_ context.Context, login, threadID string) (*store.Goal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h.init()
	if _, err := s.ownedLocked(login, threadID); err != nil {
		return nil, err
	}
	g := s.h.goals[threadID]
	if g == nil {
		return nil, store.ErrNotFound
	}
	return copyGoal(g), nil
}

func (s *Store) ClearGoal(_ context.Context, login, threadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h.init()
	if _, err := s.ownedLocked(login, threadID); err != nil {
		return err
	}
	g := s.h.goals[threadID]
	if g == nil {
		return store.ErrNotFound
	}
	g.Status, g.UpdatedAt = store.GoalCleared, s.now()
	return nil
}

func (s *Store) ThreadGoal(_ context.Context, threadID string) (*store.Goal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h.init()
	return copyGoal(s.h.goals[threadID]), nil
}

func (s *Store) UpdateGoal(_ context.Context, threadID string, u store.GoalUpdate) (*store.Goal, error) {
	if badText(u.Verdict.Reason) {
		return nil, ErrInvalidText
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h.init()
	g := s.h.goals[threadID]
	if g == nil || g.ID != u.ID || g.Status != store.GoalActive {
		return copyGoal(g), nil
	}
	v := u.Verdict
	g.LastVerdict = &v
	if u.EvalFailed {
		g.EvalFailures++
	} else {
		g.EvalFailures = 0
	}
	if u.Status != "" {
		g.Status = u.Status
	}
	g.UpdatedAt = s.now()
	return copyGoal(g), nil
}

// feedbackTargetLocked finds the visible assistant message messageID of the user's threads.
func (s *Store) feedbackTargetLocked(login, messageID string) (*store.Message, bool) {
	var found *store.Message
	for _, m := range s.messages {
		t, ok := s.threads[m.ThreadID]
		if ok && !t.deleted && t.UserLogin == login && m.ID == messageID && m.Role == store.RoleAssistant && !s.h.superseded[msgKey(m.ThreadID, m.ID)] {
			found = m
		}
	}
	return found, found != nil
}

func (s *Store) SetFeedback(_ context.Context, login, messageID string, f store.Feedback) error {
	if badText(f.Sentiment, f.Comment) {
		return ErrInvalidText
	}
	if f.Sentiment != "like" && f.Sentiment != "dislike" {
		return store.ErrInvalidData // CHECK (sentiment IN ('like', 'dislike'))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h.init()
	m, ok := s.feedbackTargetLocked(login, messageID)
	if !ok {
		return store.ErrNotFound
	}
	if s.h.feedback[m.ThreadID] == nil {
		s.h.feedback[m.ThreadID] = map[string]store.Feedback{}
	}
	s.h.feedback[m.ThreadID][m.ID] = f
	return nil
}

func (s *Store) DeleteFeedback(_ context.Context, login, messageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h.init()
	m, ok := s.feedbackTargetLocked(login, messageID)
	if !ok {
		return store.ErrNotFound
	}
	delete(s.h.feedback[m.ThreadID], m.ID)
	return nil
}

func (s *Store) ListFeedback(_ context.Context, threadID string) (map[string]store.Feedback, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h.init()
	out := map[string]store.Feedback{}
	for id, f := range s.h.feedback[threadID] {
		out[id] = f
	}
	return out, nil
}

// Superseded reports whether a message was hidden by an edit (tests).
func (s *Store) Superseded(threadID, messageID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h.init()
	return s.h.superseded[msgKey(threadID, messageID)]
}

// RecordCompaction implements store.HarnessStore.
func (s *Store) RecordCompaction(_ context.Context, c store.Compaction) error {
	if c.Trigger != "auto" && c.Trigger != "manual" {
		return store.ErrInvalidData // CHECK (trigger IN ('auto', 'manual'))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h.init()
	for i := len(s.messages) - 1; i >= 0; i-- {
		m := s.messages[i]
		if m.ThreadID != c.ThreadID || s.h.superseded[msgKey(m.ThreadID, m.ID)] {
			continue
		}
		c.ID, c.AfterMessageID, c.CreatedAt = uuid.NewString(), m.ID, s.now()
		if _, ok := s.runs[c.RunID]; !ok {
			c.RunID = ""
		}
		s.h.compactions = append(s.h.compactions, c)
		return nil
	}
	return nil
}

// ListCompactions implements store.HarnessStore.
func (s *Store) ListCompactions(_ context.Context, threadID string) ([]store.Compaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h.init()
	var out []store.Compaction
	for _, c := range s.h.compactions {
		if c.ThreadID == threadID && !s.h.superseded[msgKey(c.ThreadID, c.AfterMessageID)] {
			out = append(out, c)
		}
	}
	return out, nil
}

var _ store.HistoryStore = (*Store)(nil)

// SupersedeFrom implements store.HistoryStore.
func (s *Store) SupersedeFrom(_ context.Context, login, threadID, messageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.h.init()
	t, err := s.ownedLocked(login, threadID)
	if err != nil {
		return err
	}
	if s.activeRunLocked(t.ID) != nil {
		return store.ErrRunActive
	}
	var from int64 = -1
	for _, m := range s.messages {
		if m.ThreadID == t.ID && m.ID == messageID && m.Role == store.RoleUser && !s.h.superseded[msgKey(m.ThreadID, m.ID)] {
			from = m.Position
		}
	}
	if from < 0 {
		return store.ErrInvalidReplace
	}
	var visible []*store.Message
	for _, m := range s.messages {
		if m.ThreadID != t.ID {
			continue
		}
		if m.Position >= from {
			s.h.superseded[msgKey(m.ThreadID, m.ID)] = true
		} else if !s.h.superseded[msgKey(m.ThreadID, m.ID)] {
			visible = append(visible, m)
		}
	}
	if sum, ok := s.h.summaries[t.ID]; ok && sum.ThroughPosition >= from {
		delete(s.h.summaries, t.ID)
	}
	t.Plan = store.LatestPlan(visible)
	t.PendingQuestion = nil
	return nil
}
