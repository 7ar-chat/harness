package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/GhaziBenDahmane/harness/core/store"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func init() {
	store.RegisterDataError(func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && (strings.HasPrefix(pgErr.Code, "22") || strings.HasPrefix(pgErr.Code, "23"))
	})
}

// PG is the Postgres implementation of Store.
type PG struct {
	pool *pgxpool.Pool
	// lockSlots bounds the connections held by advisory locks (half the pool).
	lockSlots chan struct{}
}

// NewPG wraps a pool.
func NewPG(pool *pgxpool.Pool) *PG {
	return &PG{pool: pool, lockSlots: make(chan struct{}, max(1, int(pool.Config().MaxConns)/2))}
}

// NewPool opens a pgx pool for the DSN (connections are lazy); ping=true checks connectivity.
func NewPool(ctx context.Context, dsn string, maxConns int, ping bool) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing pool config: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = int32(maxConns)
	}
	cfg.HealthCheckPeriod = 30 * time.Second
	cfg.MaxConnLifetime = 10 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("creating pool: %w", err)
	}
	if !ping {
		return pool, nil
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}
	return pool, nil
}

func (s *PG) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

const threadCols = `t.id::text, t.user_login, t.title, t.pinned_at, coalesce(t.sandbox_id, ''), t.created_at, t.updated_at,
	ar.id::text, ar.status, ` + threadHarnessCols + `, ` + threadNotifyCols

const threadFrom = ` FROM work_threads t
	LEFT JOIN work_runs ar ON ar.thread_id = t.id AND ar.status IN ('queued', 'running')` + threadHarnessFrom + threadNotifyFrom

func scanThread(row pgx.Row) (*store.Thread, error) {
	var t store.Thread
	var arID, arStatus *string
	var x threadHarness
	var n threadNotify
	dest := append([]any{&t.ID, &t.UserLogin, &t.Title, &t.PinnedAt, &t.SandboxID, &t.CreatedAt, &t.UpdatedAt, &arID, &arStatus}, x.dest()...)
	if err := row.Scan(append(dest, n.dest()...)...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	if arID != nil {
		t.ActiveRun = &store.ActiveRun{ID: *arID, Status: *arStatus}
	}
	x.apply(&t)
	n.apply(&t)
	return &t, nil
}

func (s *PG) CreateThread(ctx context.Context, login, title string) (*store.Thread, error) {
	id := uuid.NewString()
	if _, err := s.pool.Exec(ctx, `INSERT INTO work_threads (id, user_login, title, title_source) VALUES ($1, $2, $3, $4)`, id, login, store.CleanText(title), titleSourceOf(title)); err != nil {
		return nil, err
	}
	return s.GetThread(ctx, login, id)
}

func (s *PG) GetThread(ctx context.Context, login, id string) (*store.Thread, error) {
	if !validUUID(id) {
		return nil, store.ErrNotFound
	}
	return scanThread(s.pool.QueryRow(ctx, `SELECT `+threadCols+threadFrom+`
		WHERE t.id = $1 AND t.user_login = $2 AND t.deleted_at IS NULL`, id, login))
}

func (s *PG) ListThreads(ctx context.Context, login string, limit int, before *store.ThreadCursor) ([]*store.Thread, error) {
	return s.SearchThreads(ctx, login, "", limit, before)
}

func (s *PG) UpdateThread(ctx context.Context, login, id string, patch store.ThreadPatch) (*store.Thread, error) {
	if !validUUID(id) {
		return nil, store.ErrNotFound
	}
	if patch.Title != nil {
		t := store.CleanText(*patch.Title)
		patch.Title = &t
	}
	tag, err := s.pool.Exec(ctx, `UPDATE work_threads SET
			title = coalesce($3, title),
			title_source = CASE WHEN $3::text IS NOT NULL THEN 'user' ELSE title_source END,
			updated_at = CASE WHEN $3::text IS NOT NULL THEN now() ELSE updated_at END,
			pinned_at = CASE WHEN $4::boolean IS NULL THEN pinned_at
			                 WHEN $4::boolean THEN coalesce(pinned_at, now())
			                 ELSE NULL END
		WHERE id = $1 AND user_login = $2 AND deleted_at IS NULL`, id, login, patch.Title, patch.Pinned)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, store.ErrNotFound
	}
	return s.GetThread(ctx, login, id)
}

// DeleteThread soft-deletes first (the UPDATE locks the row, so a concurrent
// CreateRun either committed before, and its run is reported as ActiveRun for
// the caller to cancel, or finds the thread deleted), then reads the thread as
// it was (sandbox, active run).
func (s *PG) DeleteThread(ctx context.Context, login, id string) (*store.Thread, error) {
	if !validUUID(id) {
		return nil, store.ErrNotFound
	}
	var sandboxID string
	err := s.pool.QueryRow(ctx, `UPDATE work_threads t SET deleted_at = now(), sandbox_id = NULL
		FROM (SELECT id, coalesce(sandbox_id, '') AS sandbox_id FROM work_threads WHERE id = $1 FOR UPDATE) old
		WHERE t.id = old.id AND t.user_login = $2 AND t.deleted_at IS NULL
		RETURNING old.sandbox_id`, id, login).Scan(&sandboxID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t, err := scanThread(s.pool.QueryRow(ctx, `SELECT `+threadCols+threadFrom+` WHERE t.id = $1`, id))
	if err != nil {
		return nil, err
	}
	t.SandboxID = sandboxID
	return t, nil
}

const runCols = `id::text, thread_id::text, user_login, status, model, coalesce(error, ''), coalesce(error_code, ''), cancel_requested,
	coalesce(runtime_id, ''), heartbeat_at, last_seq, created_at, started_at, finished_at, kind, coalesce(reasoning_effort, ''),
	coalesce(interaction_mode, ''), approved_tools, coalesce(sandbox_mode, ''), coalesce(run_mode, '')`

func scanRun(row pgx.Row) (*store.Run, error) {
	var r store.Run
	if err := row.Scan(&r.ID, &r.ThreadID, &r.UserLogin, &r.Status, &r.Model, &r.Error, &r.ErrorCode, &r.CancelRequested,
		&r.RuntimeID, &r.HeartbeatAt, &r.LastSeq, &r.CreatedAt, &r.StartedAt, &r.FinishedAt, &r.Kind, &r.ReasoningEffort,
		&r.InteractionMode, &r.ApprovedTools, &r.Sandbox, &r.Mode); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return &r, nil
}

func (s *PG) CreateRun(ctx context.Context, in store.NewRun) (*store.Run, *store.Message, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `UPDATE work_threads SET updated_at = now(),
			title = CASE WHEN title = '' THEN $2 ELSE title END,
			interaction_mode = coalesce(nullif($3, ''), interaction_mode)
		WHERE id = $1 AND deleted_at IS NULL`, in.ThreadID, store.CleanText(in.AutoTitle), in.InteractionMode)
	if err != nil {
		return nil, nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, nil, store.ErrNotFound
	}
	run, err := scanRun(tx.QueryRow(ctx, `INSERT INTO work_runs (id, thread_id, user_login, status, model, kind, reasoning_effort, interaction_mode, sandbox_mode, run_mode)
		VALUES ($1, $2, $3, 'queued', $4, $5, nullif($6, ''),
			coalesce(nullif($7, ''), (SELECT interaction_mode FROM work_threads WHERE id = $2), 'automatic'),
			coalesce(nullif($8, ''), (SELECT sandbox_mode FROM work_runs WHERE thread_id = $2 ORDER BY created_at DESC LIMIT 1)),
			coalesce(nullif($9, ''), (SELECT run_mode FROM work_runs WHERE thread_id = $2 ORDER BY created_at DESC LIMIT 1))) RETURNING `+runCols,
		in.RunID, in.ThreadID, in.UserLogin, in.Model, runKind(in.Kind), store.CleanText(in.ReasoningEffort), in.InteractionMode, in.Sandbox, in.Mode))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, nil, store.ErrRunActive
		}
		return nil, nil, err
	}
	if err := setRunSkills(ctx, tx, run.ID, in.Skills); err != nil {
		return nil, nil, err
	}
	if err := s.prepareRun(ctx, tx, in, run.ID); err != nil {
		return nil, nil, err
	}
	var stored *store.Message
	if in.Message.Role != "" {
		msg := in.Message
		msg.ThreadID = in.ThreadID
		msg.RunID = run.ID
		if err := insertMessage(ctx, tx, &msg); err != nil {
			if isUniqueViolation(err) {
				return nil, nil, store.ErrConflict
			}
			return nil, nil, err
		}
		stored = &msg
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return run, stored, nil
}

func (s *PG) GetRun(ctx context.Context, id string) (*store.Run, error) {
	if !validUUID(id) {
		return nil, store.ErrNotFound
	}
	return scanRun(s.pool.QueryRow(ctx, `SELECT `+runCols+` FROM work_runs WHERE id = $1`, id))
}

func (s *PG) GetUserRun(ctx context.Context, login, id string) (*store.Run, error) {
	if !validUUID(id) {
		return nil, store.ErrNotFound
	}
	return scanRun(s.pool.QueryRow(ctx, `SELECT `+runCols+` FROM work_runs WHERE id = $1 AND user_login = $2
		AND EXISTS (SELECT 1 FROM work_threads t WHERE t.id = work_runs.thread_id AND t.deleted_at IS NULL)`, id, login))
}

func (s *PG) ClaimRun(ctx context.Context, id, runtimeID string) (*store.Run, error) {
	if !validUUID(id) {
		return nil, store.ErrNotClaimable
	}
	// A queued run of a deleted thread is cancelled, never executed.
	if _, err := s.pool.Exec(ctx, `UPDATE work_runs SET status = 'cancelled', cancel_requested = true, finished_at = now()
		WHERE id = $1 AND status = 'queued'
			AND EXISTS (SELECT 1 FROM work_threads t WHERE t.id = work_runs.thread_id AND t.deleted_at IS NOT NULL)`, id); err != nil {
		return nil, err
	}
	r, err := scanRun(s.pool.QueryRow(ctx, `UPDATE work_runs
		SET status = 'running', runtime_id = $2, started_at = now(), heartbeat_at = now()
		WHERE id = $1 AND status = 'queued'
			AND NOT EXISTS (SELECT 1 FROM work_threads t WHERE t.id = work_runs.thread_id AND t.deleted_at IS NOT NULL)
		RETURNING `+runCols, id, runtimeID))
	if errors.Is(err, store.ErrNotFound) {
		return nil, store.ErrNotClaimable
	}
	return r, err
}

func (s *PG) Heartbeat(ctx context.Context, id, runtimeID string) (bool, bool, error) {
	var cancel bool
	err := s.pool.QueryRow(ctx, `UPDATE work_runs SET heartbeat_at = now()
		WHERE id = $1 AND runtime_id = $2 AND status = 'running' RETURNING cancel_requested`, id, runtimeID).Scan(&cancel)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return true, false, err
	}
	return true, cancel, nil
}

func (s *PG) RequestCancel(ctx context.Context, id string) (*store.Run, error) {
	return scanRun(s.pool.QueryRow(ctx, `UPDATE work_runs SET cancel_requested = true WHERE id = $1 RETURNING `+runCols, id))
}

func (s *PG) CancelQueuedRun(ctx context.Context, id string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE work_runs SET status = 'cancelled', cancel_requested = true, finished_at = now()
		WHERE id = $1 AND status = 'queued'`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *PG) FinishRun(ctx context.Context, id, runtimeID, status, errCode, errMsg string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE work_runs SET status = $3, error = nullif($4, ''), error_code = nullif($5, ''), finished_at = now()
		WHERE id = $1 AND runtime_id = $2 AND status = 'running'`, id, runtimeID, status, store.CleanText(errMsg), store.ErrorCodeFor(status, errCode))
	if err == nil && tag.RowsAffected() == 0 {
		return store.ErrNotOwned
	}
	return err
}

func (s *PG) ListQueuedRunsOlderThan(ctx context.Context, age time.Duration) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM work_runs
		WHERE status = 'queued' AND created_at < now() - make_interval(secs => $1) ORDER BY created_at LIMIT 500`, age.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (s *PG) InterruptStaleRuns(ctx context.Context, age time.Duration, alive ...func(*store.Run) bool) ([]*store.Run, error) {
	// The alive checks call Redis: they run on an unlocked read, never while
	// holding row locks; the update re-checks the heartbeat, so a run whose
	// runtime heartbeated meanwhile is left running (review round 1).
	candidates, err := collectRuns(s.pool.Query(ctx, `SELECT `+runCols+` FROM work_runs
		WHERE status = 'running' AND heartbeat_at < now() - make_interval(secs => $1)`, age.Seconds()))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, r := range candidates {
		if !store.Vouched(r, alive) {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return collectRuns(s.pool.Query(ctx, `UPDATE work_runs SET status = 'interrupted', finished_at = now(), error_code = 'interrupted',
			error = 'The run was interrupted because its runtime stopped responding.'
		WHERE id = ANY($1::uuid[]) AND status = 'running' AND heartbeat_at < now() - make_interval(secs => $2)
		RETURNING `+runCols, ids, age.Seconds()))
}

func collectRuns(rows pgx.Rows, err error) ([]*store.Run, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PG) ListMessages(ctx context.Context, threadID string) ([]*store.Message, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, thread_id::text, coalesce(run_id::text, ''), position, role, content,
			tool_calls, coalesce(tool_call_id, ''), coalesce(error, ''), metadata, reasoning_items, created_at,
			coalesce(author_login, ''), coalesce(author_kind, ''), coalesce(on_behalf_of, '')
		FROM work_messages WHERE thread_id = $1 AND superseded_at IS NULL ORDER BY position`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.Message
	for rows.Next() {
		var m store.Message
		var toolCalls, metadata, reasoningItems []byte
		if err := rows.Scan(&m.ID, &m.ThreadID, &m.RunID, &m.Position, &m.Role, &m.Content, &toolCalls,
			&m.ToolCallID, &m.Error, &metadata, &reasoningItems, &m.CreatedAt,
			&m.AuthorLogin, &m.AuthorKind, &m.OnBehalfOf); err != nil {
			return nil, err
		}
		if len(toolCalls) > 0 {
			_ = json.Unmarshal(toolCalls, &m.ToolCalls)
		}
		if len(metadata) > 0 {
			_ = json.Unmarshal(metadata, &m.Metadata)
		}
		if len(reasoningItems) > 0 {
			m.ReasoningItems = reasoningItems
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

type execer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func insertMessage(ctx context.Context, db execer, m *store.Message) error {
	if m.ID == "" {
		m.ID = uuid.NewString()
	}
	m.Clean()
	var toolCalls, metadata []byte
	if len(m.ToolCalls) > 0 {
		toolCalls, _ = json.Marshal(m.ToolCalls)
	}
	if len(m.Metadata) > 0 {
		metadata, _ = json.Marshal(m.Metadata)
	}
	var runID *string
	if m.RunID != "" {
		runID = &m.RunID
	}
	var reasoningItems []byte
	if len(m.ReasoningItems) > 0 && json.Valid(m.ReasoningItems) {
		reasoningItems = m.ReasoningItems
	}
	return db.QueryRow(ctx, `INSERT INTO work_messages (id, thread_id, run_id, role, content, tool_calls, tool_call_id, error, metadata, reasoning_items, author_login, author_kind, on_behalf_of)
		VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''), nullif($8, ''), $9, $10, nullif($11, ''), nullif($12, ''), nullif($13, '')) RETURNING position, created_at`,
		m.ID, m.ThreadID, runID, m.Role, m.Content, toolCalls, m.ToolCallID, m.Error, metadata, reasoningItems,
		m.AuthorLogin, m.AuthorKind, m.OnBehalfOf).Scan(&m.Position, &m.CreatedAt)
}

func (s *PG) AppendMessages(ctx context.Context, msgs ...*store.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, m := range msgs {
		if err := insertMessage(ctx, tx, m); err != nil {
			if isUniqueViolation(err) {
				return store.ErrConflict
			}
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *PG) AppendEvents(ctx context.Context, runID string, events []store.Event) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	batch := &pgx.Batch{}
	maxSeq := int64(0)
	for _, e := range events {
		batch.Queue(`INSERT INTO work_run_events (run_id, seq, type, payload) VALUES ($1, $2, $3, $4)
			ON CONFLICT (run_id, seq) DO NOTHING`, runID, e.Seq, store.CleanText(e.Type), store.CleanJSON(e.Data))
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}
	batch.Queue(`UPDATE work_runs SET last_seq = greatest(last_seq, $2) WHERE id = $1`, runID, maxSeq)
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PG) ListEvents(ctx context.Context, runID string, afterSeq int64, limit int) ([]store.Event, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT seq, type, payload FROM work_run_events
		WHERE run_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`, runID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Event
	for rows.Next() {
		var e store.Event
		var data []byte
		if err := rows.Scan(&e.Seq, &e.Type, &data); err != nil {
			return nil, err
		}
		e.Data = data
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *PG) ThreadSandbox(ctx context.Context, threadID string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT coalesce(sandbox_id, '') FROM work_threads WHERE id = $1 AND deleted_at IS NULL`, threadID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", store.ErrNotFound
	}
	return id, err
}

func (s *PG) SetThreadSandbox(ctx context.Context, threadID, expected, sandboxID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE work_threads SET sandbox_id = nullif($3, '')
		WHERE id = $1 AND coalesce(sandbox_id, '') = $2 AND deleted_at IS NULL`, threadID, expected, sandboxID)
	return err
}

func (s *PG) CountUserSandboxes(ctx context.Context, login string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM work_threads
		WHERE user_login = $1 AND sandbox_id IS NOT NULL AND deleted_at IS NULL`, login).Scan(&n)
	return n, err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func validUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

func (s *PG) LastEvent(ctx context.Context, runID string) (store.Event, bool, error) {
	var e store.Event
	var data []byte
	err := s.pool.QueryRow(ctx, `SELECT seq, type, payload FROM work_run_events WHERE run_id = $1 ORDER BY seq DESC LIMIT 1`, runID).Scan(&e.Seq, &e.Type, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Event{}, false, nil
	}
	if err != nil {
		return store.Event{}, false, err
	}
	e.Data = data
	return e, true, nil
}

func (s *PG) SetRunOutcome(ctx context.Context, id, status, errCode, errMsg string) error {
	_, err := s.pool.Exec(ctx, `UPDATE work_runs SET status = $2, error_code = nullif($3, ''), error = nullif($4, ''),
			finished_at = coalesce(finished_at, now())
		WHERE id = $1`, id, status, store.ErrorCodeFor(status, errCode), store.CleanText(errMsg))
	return err
}
