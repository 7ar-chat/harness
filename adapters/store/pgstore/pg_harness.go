package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/GhaziBenDahmane/harness/core/store"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// threadHarnessCols / threadHarnessFrom extend the thread query (see scanThread).
const threadHarnessCols = `t.title_source, t.plan, t.pending_question, coalesce(t.interaction_mode, ''), ` + threadApprovalCol + `,
	g.id::text, g.text, g.status, g.turns_used, g.max_turns, g.last_verdict, g.eval_failures, g.created_at, g.updated_at`

const threadHarnessFrom = `
	LEFT JOIN work_goals g ON g.thread_id = t.id`

type threadHarness struct {
	titleSource    string
	plan, question []byte
	mode           string
	approval       []byte
	goal           goalRow
}

type goalRow struct {
	id, text, status    *string
	turnsUsed, maxTurns *int
	verdict             []byte
	failures            *int
	createdAt           *time.Time
	updatedAt           *time.Time
}

func (g *goalRow) dest() []any {
	return []any{&g.id, &g.text, &g.status, &g.turnsUsed, &g.maxTurns, &g.verdict, &g.failures, &g.createdAt, &g.updatedAt}
}

func (g *goalRow) goal(threadID string) *store.Goal {
	if g.id == nil {
		return nil
	}
	out := &store.Goal{ID: *g.id, ThreadID: threadID, Text: *g.text, Status: *g.status, TurnsUsed: *g.turnsUsed, MaxTurns: *g.maxTurns,
		EvalFailures: *g.failures, CreatedAt: *g.createdAt, UpdatedAt: *g.updatedAt}
	if len(g.verdict) > 0 {
		var v store.GoalVerdict
		if json.Unmarshal(g.verdict, &v) == nil {
			out.LastVerdict = &v
		}
	}
	return out
}

func (x *threadHarness) dest() []any {
	return append([]any{&x.titleSource, &x.plan, &x.question, &x.mode, &x.approval}, x.goal.dest()...)
}

func (x *threadHarness) apply(t *store.Thread) {
	t.TitleSource = x.titleSource
	if len(x.plan) > 0 {
		_ = json.Unmarshal(x.plan, &t.Plan)
	}
	if len(x.question) > 0 {
		var q store.PendingQuestion
		if json.Unmarshal(x.question, &q) == nil {
			t.PendingQuestion = &q
		}
	}
	t.InteractionMode = x.mode
	t.PendingApproval = approvalFromJSON(x.approval)
	t.Goal = x.goal.goal(t.ID)
}

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

// prepareRun applies the harness parts of CreateRun inside its transaction (the
// thread row is locked): edit & resend, the pending question and goal turns.
func (s *PG) prepareRun(ctx context.Context, tx pgx.Tx, in store.NewRun, runID string) error {
	var question []byte
	var through *int64
	if err := tx.QueryRow(ctx, `SELECT pending_question, summary_through_position FROM work_threads WHERE id = $1`, in.ThreadID).Scan(&question, &through); err != nil {
		return err
	}
	if in.ReplaceFromMessageID != "" {
		var pos int64
		err := tx.QueryRow(ctx, `SELECT position FROM work_messages
			WHERE thread_id = $1 AND id = $2 AND role = 'user' AND superseded_at IS NULL`, in.ThreadID, in.ReplaceFromMessageID).Scan(&pos)
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrInvalidReplace
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE work_messages SET superseded_at = now()
			WHERE thread_id = $1 AND position >= $2 AND superseded_at IS NULL`, in.ThreadID, pos); err != nil {
			return err
		}
		if through != nil && *through >= pos {
			if _, err := tx.Exec(ctx, `UPDATE work_threads SET summary = NULL, summary_through_position = NULL WHERE id = $1`, in.ThreadID); err != nil {
				return err
			}
		}
	}
	handled, err := s.prepareApproval(ctx, tx, in, runID)
	if err != nil {
		return err
	}
	if handled {
		in.Resume = nil // the resume decided the pending approval
	}
	var pending *store.PendingQuestion
	if len(question) > 0 {
		pending = &store.PendingQuestion{}
		if err := json.Unmarshal(question, pending); err != nil {
			pending = nil
		}
	}
	result, err := resumeResult(pending, in, handled)
	if err != nil {
		return err
	}
	if result != nil {
		result.RunID = runID
		if err := insertMessage(ctx, tx, result); err != nil {
			return err
		}
		if in.Resume != nil && pending != nil && pending.RunID != "" {
			// The answer continues the paused run's task: same approved tool kinds (scope run).
			if _, err := tx.Exec(ctx, `UPDATE work_runs SET approved_tools = $2 WHERE id = $1`, runID,
				pausedApprovedTools(ctx, tx, pending.RunID)); err != nil {
				return err
			}
		}
		if in.ReplaceFromMessageID != "" {
			// Hidden with the call it answers when the edit hid that call.
			if _, err := tx.Exec(ctx, `UPDATE work_messages SET superseded_at = now() WHERE thread_id = $1 AND id = $2
				AND NOT EXISTS (SELECT 1 FROM work_messages a WHERE a.thread_id = $1 AND a.role = 'assistant' AND a.superseded_at IS NULL
					AND a.tool_calls @> jsonb_build_array(jsonb_build_object('id', $3::text)))`, in.ThreadID, result.ID, result.ToolCallID); err != nil {
				return err
			}
		}
	}
	if in.ReplaceFromMessageID != "" {
		if err := s.recomputePlan(ctx, tx, in.ThreadID); err != nil {
			return err
		}
	}
	if pending != nil && !handled {
		if _, err := tx.Exec(ctx, `UPDATE work_threads SET pending_question = NULL WHERE id = $1`, in.ThreadID); err != nil {
			return err
		}
	}
	if in.GoalID != "" {
		tag, err := tx.Exec(ctx, `UPDATE work_goals SET turns_used = turns_used + 1, updated_at = now()
			WHERE thread_id = $1 AND id::text = $2 AND status = 'active' AND turns_used < max_turns`, in.ThreadID, in.GoalID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return store.ErrGoalInactive
		}
	}
	return nil
}

// resumeResult is the ask_user tool result a new run stores for the pending question (nil: none).
func resumeResult(pending *store.PendingQuestion, in store.NewRun, decided bool) (*store.Message, error) {
	switch {
	case pending == nil && in.Resume != nil:
		return nil, store.ErrNoPendingQuestion
	case pending == nil:
		return nil, nil
	case decided: // resume-only run of an approval: the question (if any) stays open
		return nil, nil
	case in.Resume != nil:
		if in.Resume.InterruptID != pending.InterruptID {
			return nil, store.ErrNoPendingQuestion
		}
		return &store.Message{ID: uuid.NewString(), ThreadID: in.ThreadID, Role: store.RoleTool, ToolCallID: pending.ToolCallID, Content: in.Resume.Content}, nil
	case in.Message.Role == "":
		return nil, store.ErrQuestionPending
	default: // a new message (also an edit) implicitly cancels the question
		return &store.Message{ID: uuid.NewString(), ThreadID: in.ThreadID, Role: store.RoleTool, ToolCallID: pending.ToolCallID, Content: store.ImplicitCancelResult}, nil
	}
}

// recomputePlan sets the thread's plan from its visible messages (after an edit hid some).
func (s *PG) recomputePlan(ctx context.Context, tx pgx.Tx, threadID string) error {
	rows, err := tx.Query(ctx, `SELECT role, tool_calls, coalesce(tool_call_id, ''), coalesce(error, '') FROM work_messages
		WHERE thread_id = $1 AND superseded_at IS NULL AND role IN ('assistant', 'tool') ORDER BY position`, threadID)
	if err != nil {
		return err
	}
	var msgs []*store.Message
	for rows.Next() {
		var m store.Message
		var calls []byte
		if err := rows.Scan(&m.Role, &calls, &m.ToolCallID, &m.Error); err != nil {
			rows.Close()
			return err
		}
		if len(calls) > 0 {
			_ = json.Unmarshal(calls, &m.ToolCalls)
		}
		msgs = append(msgs, &m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var plan []byte
	if items := store.LatestPlan(msgs); items != nil {
		plan, _ = json.Marshal(items)
		plan = store.CleanJSON(plan)
	}
	_, err = tx.Exec(ctx, `UPDATE work_threads SET plan = $2 WHERE id = $1`, threadID, plan)
	return err
}

// likePattern is an ILIKE pattern matching s literally anywhere (escape character \).
func likePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

func (s *PG) SearchThreads(ctx context.Context, login, search string, limit int, before *store.ThreadCursor) ([]*store.Thread, error) {
	return s.searchThreads(ctx, login, search, false, limit, before)
}

func (s *PG) searchThreads(ctx context.Context, login, search string, unread bool, limit int, before *store.ThreadCursor) ([]*store.Thread, error) {
	args := []any{login, limit}
	where := `t.user_login = $1 AND t.deleted_at IS NULL`
	if unread {
		where += ` AND ` + threadUnreadCond
	}
	if before != nil {
		// Row-value comparison on (pinned, created_at, id) in the list order (all DESC).
		where += ` AND ((t.pinned_at IS NOT NULL), t.created_at, t.id) < ($3::boolean, $4::timestamptz, $5::uuid)`
		id := before.ID
		if !validUUID(id) {
			id = "ffffffff-ffff-ffff-ffff-ffffffffffff"
		}
		args = append(args, before.Pinned, before.CreatedAt, id)
	}
	if search != "" {
		args = append(args, likePattern(store.CleanText(search)))
		n := strconv.Itoa(len(args))
		where += ` AND (t.title ILIKE $` + n + ` ESCAPE '\' OR EXISTS (SELECT 1 FROM work_messages m
			WHERE m.thread_id = t.id AND m.superseded_at IS NULL AND m.role IN ('user', 'assistant') AND m.content ILIKE $` + n + ` ESCAPE '\'))`
	}
	rows, err := s.pool.Query(ctx, `SELECT `+threadCols+threadFrom+` WHERE `+where+`
		ORDER BY (t.pinned_at IS NOT NULL) DESC, t.created_at DESC, t.id DESC LIMIT $2`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.Thread
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PG) GetThreadSummary(ctx context.Context, threadID string) (store.ThreadSummary, error) {
	var out store.ThreadSummary
	err := s.pool.QueryRow(ctx, `SELECT coalesce(summary, ''), coalesce(summary_through_position, 0) FROM work_threads WHERE id = $1`, threadID).
		Scan(&out.Text, &out.ThroughPosition)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ThreadSummary{}, store.ErrNotFound
	}
	return out, err
}

func (s *PG) SetThreadSummary(ctx context.Context, threadID string, sum store.ThreadSummary) error {
	if sum.Text == "" {
		_, err := s.pool.Exec(ctx, `UPDATE work_threads SET summary = NULL, summary_through_position = NULL WHERE id = $1`, threadID)
		return err
	}
	_, err := s.pool.Exec(ctx, `UPDATE work_threads SET summary = $2, summary_through_position = $3 WHERE id = $1`, threadID, store.CleanText(sum.Text), sum.ThroughPosition)
	return err
}

func (s *PG) SetThreadPlan(ctx context.Context, threadID string, items []store.PlanItem) error {
	if items == nil {
		items = []store.PlanItem{}
	}
	b, _ := json.Marshal(items)
	_, err := s.pool.Exec(ctx, `UPDATE work_threads SET plan = $2 WHERE id = $1`, threadID, store.CleanJSON(b))
	return err
}

func (s *PG) SetPendingQuestion(ctx context.Context, threadID string, q *store.PendingQuestion) error {
	var b []byte
	if q != nil {
		b, _ = json.Marshal(q)
		b = store.CleanJSON(b)
	}
	_, err := s.pool.Exec(ctx, `UPDATE work_threads SET pending_question = $2 WHERE id = $1`, threadID, b)
	return err
}

func (s *PG) SetGeneratedTitle(ctx context.Context, threadID, title string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE work_threads SET title = $2, title_source = 'generated'
		WHERE id = $1 AND title_source = 'auto' AND deleted_at IS NULL`, threadID, store.CleanText(title))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

const goalCols = `g.id::text, g.text, g.status, g.turns_used, g.max_turns, g.last_verdict, g.eval_failures, g.created_at, g.updated_at`

func scanGoal(row pgx.Row, threadID string) (*store.Goal, error) {
	var g goalRow
	if err := row.Scan(g.dest()...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return g.goal(threadID), nil
}

func (s *PG) PutGoal(ctx context.Context, login, threadID, text string, maxTurns int) (*store.Goal, error) {
	if !validUUID(threadID) {
		return nil, store.ErrNotFound
	}
	return scanGoal(s.pool.QueryRow(ctx, `INSERT INTO work_goals AS g (thread_id, id, text, status, max_turns)
		SELECT t.id, $3, $4, 'active', $5 FROM work_threads t WHERE t.id = $1 AND t.user_login = $2 AND t.deleted_at IS NULL
		ON CONFLICT (thread_id) DO UPDATE SET id = excluded.id, text = excluded.text, status = 'active', turns_used = 0,
			max_turns = excluded.max_turns, last_verdict = NULL, eval_failures = 0, created_at = now(), updated_at = now()
		RETURNING `+goalCols, threadID, login, uuid.NewString(), store.CleanText(text), maxTurns), threadID)
}

func (s *PG) GetGoal(ctx context.Context, login, threadID string) (*store.Goal, error) {
	if !validUUID(threadID) {
		return nil, store.ErrNotFound
	}
	return scanGoal(s.pool.QueryRow(ctx, `SELECT `+goalCols+` FROM work_goals g JOIN work_threads t ON t.id = g.thread_id
		WHERE g.thread_id = $1 AND t.user_login = $2 AND t.deleted_at IS NULL`, threadID, login), threadID)
}

func (s *PG) ClearGoal(ctx context.Context, login, threadID string) error {
	if !validUUID(threadID) {
		return store.ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE work_goals g SET status = 'cleared', updated_at = now() FROM work_threads t
		WHERE g.thread_id = t.id AND t.id = $1 AND t.user_login = $2 AND t.deleted_at IS NULL`, threadID, login)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *PG) ThreadGoal(ctx context.Context, threadID string) (*store.Goal, error) {
	g, err := scanGoal(s.pool.QueryRow(ctx, `SELECT `+goalCols+` FROM work_goals g WHERE g.thread_id = $1`, threadID), threadID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return g, err
}

func (s *PG) UpdateGoal(ctx context.Context, threadID string, u store.GoalUpdate) (*store.Goal, error) {
	v := u.Verdict
	v.Reason = store.CleanText(v.Reason)
	b, _ := json.Marshal(v)
	g, err := scanGoal(s.pool.QueryRow(ctx, `UPDATE work_goals g SET last_verdict = $3,
			eval_failures = CASE WHEN $4 THEN eval_failures + 1 ELSE 0 END,
			status = coalesce(nullif($5, ''), status), updated_at = now()
		WHERE g.thread_id = $1 AND g.id::text = $2 AND g.status = 'active' RETURNING `+goalCols,
		threadID, u.ID, store.CleanJSON(b), u.EvalFailed, u.Status), threadID)
	if errors.Is(err, store.ErrNotFound) {
		return s.ThreadGoal(ctx, threadID)
	}
	return g, err
}

// feedbackTarget selects the visible assistant message messageId ($1) of the user's ($2) threads.
const feedbackTarget = `SELECT m.thread_id, m.id FROM work_messages m JOIN work_threads t ON t.id = m.thread_id
	WHERE m.id = $1 AND m.role = 'assistant' AND m.superseded_at IS NULL AND t.user_login = $2 AND t.deleted_at IS NULL
	ORDER BY m.created_at DESC LIMIT 1`

func (s *PG) SetFeedback(ctx context.Context, login, messageID string, f store.Feedback) error {
	tag, err := s.pool.Exec(ctx, `INSERT INTO work_message_feedback (thread_id, message_id, user_login, sentiment, comment)
		SELECT x.thread_id, x.id, $2, $3, nullif($4, '') FROM (`+feedbackTarget+`) x
		ON CONFLICT (thread_id, message_id) DO UPDATE SET sentiment = excluded.sentiment, comment = excluded.comment, updated_at = now()`,
		messageID, login, f.Sentiment, store.CleanText(f.Comment))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *PG) DeleteFeedback(ctx context.Context, login, messageID string) error {
	var threadID, id string
	err := s.pool.QueryRow(ctx, feedbackTarget, messageID, login).Scan(&threadID, &id)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM work_message_feedback WHERE thread_id = $1 AND message_id = $2`, threadID, id)
	return err
}

func (s *PG) ListFeedback(ctx context.Context, threadID string) (map[string]store.Feedback, error) {
	rows, err := s.pool.Query(ctx, `SELECT message_id, sentiment, coalesce(comment, '') FROM work_message_feedback WHERE thread_id = $1`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]store.Feedback{}
	for rows.Next() {
		var id string
		var f store.Feedback
		if err := rows.Scan(&id, &f.Sentiment, &f.Comment); err != nil {
			return nil, err
		}
		out[id] = f
	}
	return out, rows.Err()
}

// RecordCompaction implements HarnessStore.
func (s *PG) RecordCompaction(ctx context.Context, c store.Compaction) error {
	if !validUUID(c.ThreadID) {
		return nil
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO work_compactions (id, thread_id, run_id, after_message_id, trigger, tokens_before, tokens_after)
		SELECT $1, m.thread_id, $3, m.id, $4, $5, $6 FROM work_messages m
		WHERE m.thread_id = $2 AND m.superseded_at IS NULL ORDER BY m.position DESC LIMIT 1`,
		uuid.NewString(), c.ThreadID, nullUUID(c.RunID), c.Trigger, c.TokensBefore, c.TokensAfter)
	return err
}

// ListCompactions implements HarnessStore.
func (s *PG) ListCompactions(ctx context.Context, threadID string) ([]store.Compaction, error) {
	if !validUUID(threadID) {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT c.id::text, c.thread_id::text, coalesce(c.run_id::text, ''), c.after_message_id, c.trigger,
			c.tokens_before, c.tokens_after, c.created_at
		FROM work_compactions c JOIN work_messages m ON m.thread_id = c.thread_id AND m.id = c.after_message_id
		WHERE c.thread_id = $1 AND m.superseded_at IS NULL ORDER BY c.created_at, c.id`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Compaction
	for rows.Next() {
		var c store.Compaction
		if err := rows.Scan(&c.ID, &c.ThreadID, &c.RunID, &c.AfterMessageID, &c.Trigger, &c.TokensBefore, &c.TokensAfter, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

var _ store.HistoryStore = (*PG)(nil)

// SupersedeFrom implements store.HistoryStore.
func (s *PG) SupersedeFrom(ctx context.Context, login, threadID, messageID string) error {
	if !validUUID(threadID) {
		return store.ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var through *int64
	err = tx.QueryRow(ctx, `SELECT summary_through_position FROM work_threads
		WHERE id = $1 AND user_login = $2 AND deleted_at IS NULL FOR UPDATE`, threadID, login).Scan(&through)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return err
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_runs WHERE thread_id = $1 AND status IN ('queued', 'running'))`, threadID).Scan(&active); err != nil {
		return err
	}
	if active {
		return store.ErrRunActive
	}
	var pos int64
	err = tx.QueryRow(ctx, `SELECT position FROM work_messages
		WHERE thread_id = $1 AND id = $2 AND role = 'user' AND superseded_at IS NULL`, threadID, messageID).Scan(&pos)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrInvalidReplace
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE work_messages SET superseded_at = now()
		WHERE thread_id = $1 AND position >= $2 AND superseded_at IS NULL`, threadID, pos); err != nil {
		return err
	}
	if through != nil && *through >= pos {
		if _, err := tx.Exec(ctx, `UPDATE work_threads SET summary = NULL, summary_through_position = NULL WHERE id = $1`, threadID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE work_threads SET pending_question = NULL WHERE id = $1`, threadID); err != nil {
		return err
	}
	if err := s.recomputePlan(ctx, tx, threadID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
