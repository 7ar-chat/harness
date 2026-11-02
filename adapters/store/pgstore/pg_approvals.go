package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/7ar-chat/harness/core/store"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const approvalCols = `a.id::text, a.kind, a.thread_id::text, a.run_id::text, a.tool_call_id, a.tool_name, a.tool_input_preview, a.status,
	coalesce(a.decision, ''), coalesce(a.selected_choice, ''), coalesce(a.response_text, ''), coalesce(a.resume_run_id::text, ''),
	a.created_at, a.resolved_at, a.scope`

// pendingApprovalWhere selects the approval a thread waits for: pending, of a
// run that completed (ended on the interrupt). $1 is the thread id.
const pendingApprovalWhere = ` FROM work_approvals a JOIN work_runs r ON r.id = a.run_id
	WHERE a.thread_id = $1 AND a.status = 'pending' AND r.status = 'completed'`

// threadApprovalCol is the thread's pending approval as JSON (see threadHarness).
const threadApprovalCol = `(SELECT to_jsonb(pa) FROM work_approvals pa JOIN work_runs pr ON pr.id = pa.run_id
	WHERE pa.thread_id = t.id AND pa.status = 'pending' AND pr.status = 'completed' ORDER BY pa.created_at DESC LIMIT 1)`

func scanApproval(row pgx.Row) (*store.Approval, error) {
	var a store.Approval
	if err := row.Scan(&a.ID, &a.Kind, &a.ThreadID, &a.RunID, &a.ToolCallID, &a.ToolName, &a.ToolInputPreview, &a.Status,
		&a.Decision, &a.Response.SelectedChoice, &a.Response.ResponseText, &a.ResumeRunID, &a.CreatedAt, &a.ResolvedAt, &a.Scope); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return &a, nil
}

// approvalJSON is a work_approvals row as produced by to_jsonb.
type approvalJSON struct {
	ID               string     `json:"id"`
	Kind             string     `json:"kind"`
	ThreadID         string     `json:"thread_id"`
	RunID            string     `json:"run_id"`
	ToolCallID       string     `json:"tool_call_id"`
	ToolName         string     `json:"tool_name"`
	ToolInputPreview string     `json:"tool_input_preview"`
	Status           string     `json:"status"`
	Decision         *string    `json:"decision"`
	SelectedChoice   *string    `json:"selected_choice"`
	ResponseText     *string    `json:"response_text"`
	ResumeRunID      *string    `json:"resume_run_id"`
	CreatedAt        time.Time  `json:"created_at"`
	ResolvedAt       *time.Time `json:"resolved_at"`
	Scope            string     `json:"scope"`
}

func approvalFromJSON(b []byte) *store.Approval {
	if len(b) == 0 {
		return nil
	}
	var r approvalJSON
	if json.Unmarshal(b, &r) != nil || r.ID == "" {
		return nil
	}
	deref := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	return &store.Approval{ID: r.ID, Kind: r.Kind, ThreadID: r.ThreadID, RunID: r.RunID, ToolCallID: r.ToolCallID, ToolName: r.ToolName,
		ToolInputPreview: r.ToolInputPreview, Status: r.Status, Decision: deref(r.Decision),
		Response:    store.ApprovalResponse{SelectedChoice: deref(r.SelectedChoice), ResponseText: deref(r.ResponseText)},
		ResumeRunID: deref(r.ResumeRunID), CreatedAt: r.CreatedAt, ResolvedAt: r.ResolvedAt, Scope: r.Scope}
}

func (s *PG) CreateApproval(ctx context.Context, a *store.Approval) error {
	if a.ID == "" {
		a.ID = uuid.NewString()
	}
	if a.Kind == "" {
		a.Kind = store.ApprovalKindRiskyTool
	}
	a.Status = store.ApprovalPending
	return s.pool.QueryRow(ctx, `INSERT INTO work_approvals (id, kind, thread_id, run_id, tool_call_id, tool_name, tool_input_preview)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`,
		a.ID, a.Kind, a.ThreadID, a.RunID, store.CleanText(a.ToolCallID), store.CleanText(a.ToolName), store.CleanText(a.ToolInputPreview)).Scan(&a.CreatedAt)
}

func (s *PG) GetApproval(ctx context.Context, id string) (*store.Approval, error) {
	if !validUUID(id) {
		return nil, store.ErrNotFound
	}
	return scanApproval(s.pool.QueryRow(ctx, `SELECT `+approvalCols+` FROM work_approvals a WHERE a.id = $1`, id))
}

func (s *PG) ResumedApproval(ctx context.Context, runID string) (*store.Approval, error) {
	if !validUUID(runID) {
		return nil, nil
	}
	a, err := scanApproval(s.pool.QueryRow(ctx, `SELECT `+approvalCols+` FROM work_approvals a
		WHERE a.resume_run_id = $1 ORDER BY a.resolved_at DESC LIMIT 1`, runID))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return a, err
}

func (s *PG) CancelRunApprovals(ctx context.Context, runID string) error {
	if !validUUID(runID) {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE work_approvals SET status = 'cancelled', resolved_at = now()
		WHERE run_id = $1 AND status = 'pending'`, runID)
	return err
}

func (s *PG) CancelPendingApproval(ctx context.Context, runID, result string) (*store.Approval, error) {
	if !validUUID(runID) {
		return nil, store.ErrApprovalNotPending
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var threadID string
	if err := tx.QueryRow(ctx, `SELECT thread_id::text FROM work_runs WHERE id = $1`, runID).Scan(&threadID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrApprovalNotPending
		}
		return nil, err
	}
	// Lock the thread like CreateRun: a concurrent decision waits for us (or we for it).
	if _, err := tx.Exec(ctx, `SELECT 1 FROM work_threads WHERE id = $1 FOR UPDATE`, threadID); err != nil {
		return nil, err
	}
	a, err := scanApproval(tx.QueryRow(ctx, `SELECT `+approvalCols+pendingApprovalWhere+` AND a.run_id = $2 FOR UPDATE OF a`, threadID, runID))
	if errors.Is(err, store.ErrNotFound) {
		return nil, store.ErrApprovalNotPending
	}
	if err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx, `UPDATE work_approvals SET status = 'cancelled', resolved_at = now() WHERE id = $1 RETURNING resolved_at`, a.ID).
		Scan(&a.ResolvedAt); err != nil {
		return nil, err
	}
	a.Status = store.ApprovalCancelled
	if err := insertMessage(ctx, tx, &store.Message{ThreadID: threadID, RunID: runID, Role: store.RoleTool, ToolCallID: a.ToolCallID, Content: result}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// pausedApprovedTools are the approved tool kinds of a run (none on error: fail closed).
func pausedApprovedTools(ctx context.Context, tx pgx.Tx, runID string) []string {
	var tools []string
	if err := tx.QueryRow(ctx, `SELECT approved_tools FROM work_runs WHERE id = $1`, runID).Scan(&tools); err != nil {
		return nil
	}
	return tools
}

func (s *PG) ExpireApprovals(ctx context.Context, ttl time.Duration, threadID string) (int, error) {
	if threadID != "" && !validUUID(threadID) {
		return 0, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `UPDATE work_approvals a SET status = 'expired', resolved_at = now()
		WHERE a.status = 'pending' AND a.created_at < now() - make_interval(secs => $1) AND ($2 = '' OR a.thread_id::text = $2)
		RETURNING a.thread_id::text, a.run_id::text, a.tool_call_id,
			EXISTS (SELECT 1 FROM work_runs r WHERE r.id = a.run_id AND r.status = 'completed')`, ttl.Seconds(), threadID)
	if err != nil {
		return 0, err
	}
	type expired struct {
		thread, run, call string
		waited            bool
	}
	var done []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.thread, &e.run, &e.call, &e.waited); err != nil {
			rows.Close()
			return 0, err
		}
		done = append(done, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, e := range done {
		if !e.waited {
			continue // its run failed or was cancelled: the history marks the call interrupted
		}
		if err := insertMessage(ctx, tx, &store.Message{ThreadID: e.thread, RunID: e.run, Role: store.RoleTool, ToolCallID: e.call, Content: store.ApprovalExpiredResult}); err != nil {
			return 0, err
		}
	}
	return len(done), tx.Commit(ctx)
}

func (s *PG) ReopenApproval(ctx context.Context, id string) (*store.Approval, error) {
	if !validUUID(id) {
		return nil, store.ErrApprovalNotPending
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var threadID string
	if err := tx.QueryRow(ctx, `SELECT thread_id::text FROM work_approvals WHERE id = $1`, id).Scan(&threadID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrApprovalNotPending
		}
		return nil, err
	}
	// Lock the thread like CreateRun: no run can start while we check.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM work_threads WHERE id = $1 FOR UPDATE`, threadID); err != nil {
		return nil, err
	}
	a, err := scanApproval(tx.QueryRow(ctx, `UPDATE work_approvals a SET status = 'pending', decision = NULL, selected_choice = NULL,
			response_text = NULL, resume_run_id = NULL, resolved_at = NULL
		FROM work_runs rr, work_runs pr
		WHERE a.id = $1 AND a.status = 'resolved' AND rr.id = a.resume_run_id AND pr.id = a.run_id AND pr.status = 'completed'
			AND rr.status IN ('completed', 'failed', 'cancelled', 'interrupted')
			AND NOT EXISTS (SELECT 1 FROM work_runs o WHERE o.thread_id = a.thread_id AND o.id <> rr.id
				AND (o.status IN ('queued', 'running') OR o.created_at > rr.created_at))
			AND NOT EXISTS (SELECT 1 FROM work_messages m WHERE m.thread_id = a.thread_id AND m.role = 'tool'
				AND m.tool_call_id = a.tool_call_id AND m.superseded_at IS NULL)
		RETURNING `+approvalCols, id))
	if errors.Is(err, store.ErrNotFound) {
		return nil, store.ErrApprovalNotPending
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// prepareApproval applies a new run to the thread's pending approval inside
// CreateRun's transaction: a resume with its id decides it (deny/respond store
// their tool result now, approve lets the run execute the call), a message
// cancels it, other runs are refused. handled reports that in.Resume was used.
func (s *PG) prepareApproval(ctx context.Context, tx pgx.Tx, in store.NewRun, runID string) (handled bool, err error) {
	a, err := scanApproval(tx.QueryRow(ctx, `SELECT `+approvalCols+pendingApprovalWhere+` ORDER BY a.created_at DESC LIMIT 1 FOR UPDATE OF a`, in.ThreadID))
	if errors.Is(err, store.ErrNotFound) {
		if runKind(in.Kind) == store.KindCompact {
			return false, nil
		}
		// Approvals cancelled with their waiting run (stop) or expired are announced by the next run.
		_, err := tx.Exec(ctx, `UPDATE work_approvals a SET resume_run_id = $2 FROM work_runs r
			WHERE a.thread_id = $1 AND a.status IN ('cancelled', 'expired') AND a.resume_run_id IS NULL AND r.id = a.run_id AND r.status = 'completed'`,
			in.ThreadID, runID)
		return false, err
	}
	if err != nil {
		return false, err
	}
	var content string
	switch {
	case in.Resume != nil && in.Resume.InterruptID == a.ID:
		decision := in.Resume.Decision
		if decision == "" {
			decision = store.DecisionDeny
		}
		r := in.Resume.Response
		scope := store.ApprovalScopeOnce
		if in.Resume.Scope == store.ApprovalScopeRun && decision == store.DecisionApprove {
			scope = store.ApprovalScopeRun
		}
		if _, err := tx.Exec(ctx, `UPDATE work_approvals SET status = 'resolved', decision = $2, selected_choice = nullif($3, ''),
			response_text = nullif($4, ''), resume_run_id = $5, resolved_at = now(), scope = $6 WHERE id = $1`,
			a.ID, decision, store.CleanText(r.SelectedChoice), store.CleanText(r.ResponseText), runID, scope); err != nil {
			return false, err
		}
		a.Decision, a.Scope = decision, scope
		// The resume run inherits the paused run's approved tool kinds (+ this one for scope run).
		if _, err := tx.Exec(ctx, `UPDATE work_runs SET approved_tools = $2 WHERE id = $1`, runID,
			store.ApprovedToolsAfter(pausedApprovedTools(ctx, tx, a.RunID), a)); err != nil {
			return false, err
		}
		handled, content = true, store.ApprovalResult(decision, r)
	case in.Resume != nil:
		return false, nil // not this approval: the question logic refuses it
	case in.Message.Role == "":
		return false, store.ErrApprovalPending
	default: // a new message (also an edit) implicitly cancels the approval
		if _, err := tx.Exec(ctx, `UPDATE work_approvals SET status = 'cancelled', resume_run_id = $2, resolved_at = now() WHERE id = $1`, a.ID, runID); err != nil {
			return false, err
		}
		content = store.ImplicitApprovalCancelResult
	}
	if content == "" {
		return handled, nil
	}
	result := &store.Message{ThreadID: in.ThreadID, RunID: runID, Role: store.RoleTool, ToolCallID: a.ToolCallID, Content: content}
	if err := insertMessage(ctx, tx, result); err != nil {
		return false, err
	}
	return handled, hideOrphanResult(ctx, tx, in, result)
}

// hideOrphanResult hides a stored tool result with the call it answers when an edit hid that call.
func hideOrphanResult(ctx context.Context, tx pgx.Tx, in store.NewRun, result *store.Message) error {
	if in.ReplaceFromMessageID == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE work_messages SET superseded_at = now() WHERE thread_id = $1 AND id = $2
		AND NOT EXISTS (SELECT 1 FROM work_messages a WHERE a.thread_id = $1 AND a.role = 'assistant' AND a.superseded_at IS NULL
			AND a.tool_calls @> jsonb_build_array(jsonb_build_object('id', $3::text)))`, in.ThreadID, result.ID, result.ToolCallID)
	return err
}

func (s *PG) LatestExpiredApproval(ctx context.Context, threadID string) (*store.Approval, error) {
	if !validUUID(threadID) {
		return nil, nil
	}
	a, err := scanApproval(s.pool.QueryRow(ctx, `SELECT `+approvalCols+` FROM work_approvals a
		WHERE a.id = (SELECT l.id FROM work_approvals l WHERE l.thread_id = $1 ORDER BY l.created_at DESC, l.id DESC LIMIT 1)
			AND a.status = 'expired'
			AND a.run_id = (SELECT r.id FROM work_runs r WHERE r.thread_id = $1 ORDER BY r.created_at DESC, r.id DESC LIMIT 1)`, threadID))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return a, err
}

// SetThreadInteractionMode implements ApprovalStore. The thread row is locked
// first, like CreateRun does, so a run created concurrently either is already
// active (and switched here) or inherits the new mode from the thread.
func (s *PG) SetThreadInteractionMode(ctx context.Context, login, threadID, mode string) error {
	if !validUUID(threadID) {
		return store.ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `UPDATE work_threads SET interaction_mode = $3
		WHERE id = $1 AND user_login = $2 AND deleted_at IS NULL`, threadID, login, mode)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE work_runs SET interaction_mode = $2
		WHERE thread_id = $1 AND status IN ('queued', 'running')`, threadID, mode); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RunInteractionMode implements ApprovalStore.
func (s *PG) RunInteractionMode(ctx context.Context, runID string) (string, error) {
	if !validUUID(runID) {
		return "", store.ErrNotFound
	}
	var mode string
	err := s.pool.QueryRow(ctx, `SELECT coalesce(interaction_mode, '') FROM work_runs WHERE id = $1`, runID).Scan(&mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", store.ErrNotFound
	}
	return mode, err
}
