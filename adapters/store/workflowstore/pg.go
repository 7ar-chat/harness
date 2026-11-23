package workflowstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/core/workflows"
)

// PG is a workflows.Store on Postgres (tables of migration 0600, applied by pgstore). Several processes may
// share it: the name is unique per project, an update locks the row, and a run changes only from the version
// the caller read.
type PG struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPG returns a store on pool, whose search_path names the schema the migrations ran in. now (nil: time.Now)
// is the clock.
func NewPG(pool *pgxpool.Pool, now func() time.Time) *PG {
	if now == nil {
		now = time.Now
	}
	return &PG{pool: pool, now: now}
}

var _ workflows.Store = (*PG)(nil)

func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (s *PG) stamp() time.Time { return s.now().UTC().Truncate(time.Second) }

func decode[T any](b []byte) (T, error) {
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return v, fmt.Errorf("workflowstore: decode: %w", err)
	}
	return v, nil
}

// Create implements workflows.Store.
func (s *PG) Create(ctx context.Context, w workflows.Workflow, actor string) (*workflows.Workflow, error) {
	if err := prepare(&w); err != nil {
		return nil, err
	}
	if w.ID == "" {
		w.ID = uuid.NewString()
	}
	now := s.stamp()
	w.Version, w.CreatedBy, w.UpdatedBy, w.CreatedAt, w.UpdatedAt = 1, actor, actor, now, now
	doc, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("workflowstore: encode: %w", err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO work_workflows (id, project_id, name, version, doc, created_at, updated_at) VALUES ($1, $2, $3, 1, $4, $5, $5)`,
		w.ID, w.ProjectID, w.Name, doc, now)
	if isUnique(err) {
		return nil, workflows.ErrConflict
	}
	if err != nil {
		return nil, fmt.Errorf("workflowstore: create: %w", err)
	}
	return &w, nil
}

// Update implements workflows.Store.
func (s *PG) Update(ctx context.Context, w workflows.Workflow, actor string) (*workflows.Workflow, error) {
	if err := prepare(&w); err != nil {
		return nil, err
	}
	now := s.stamp()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("workflowstore: begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT doc FROM work_workflows WHERE id = $1 FOR UPDATE`, w.ID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, workflows.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("workflowstore: read: %w", err)
	}
	cur, err := decode[workflows.Workflow](raw)
	if err != nil {
		return nil, err
	}
	if w.Version != 0 && w.Version != cur.Version {
		return nil, workflows.ErrStale
	}
	if _, err := tx.Exec(ctx, `INSERT INTO work_workflow_versions (workflow_id, version, doc, changed_by, at) VALUES ($1, $2, $3, $4, $5)`,
		w.ID, cur.Version, raw, cur.UpdatedBy, cur.UpdatedAt); err != nil {
		return nil, fmt.Errorf("workflowstore: keep version: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM work_workflow_versions WHERE workflow_id = $1
		AND version NOT IN (SELECT version FROM work_workflow_versions WHERE workflow_id = $1 ORDER BY version DESC LIMIT $2)`,
		w.ID, workflows.MaxHistory); err != nil {
		return nil, fmt.Errorf("workflowstore: trim history: %w", err)
	}
	w.ProjectID, w.Template = cur.ProjectID, cur.Template
	w.Version = cur.Version + 1
	w.CreatedBy, w.CreatedAt, w.UpdatedBy, w.UpdatedAt = cur.CreatedBy, cur.CreatedAt, actor, now
	doc, err := json.Marshal(w)
	if err != nil {
		return nil, fmt.Errorf("workflowstore: encode: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE work_workflows SET name = $2, version = $3, doc = $4, updated_at = $5 WHERE id = $1`,
		w.ID, w.Name, w.Version, doc, now); err != nil {
		if isUnique(err) {
			return nil, workflows.ErrConflict
		}
		return nil, fmt.Errorf("workflowstore: update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("workflowstore: commit: %w", err)
	}
	return &w, nil
}

// Get implements workflows.Store.
func (s *PG) Get(ctx context.Context, id string) (*workflows.Workflow, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT doc FROM work_workflows WHERE id = $1`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, workflows.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("workflowstore: get: %w", err)
	}
	w, err := decode[workflows.Workflow](raw)
	return &w, err
}

// List implements workflows.Store.
func (s *PG) List(ctx context.Context, projectID string) ([]workflows.Workflow, error) {
	rows, err := s.pool.Query(ctx, `SELECT doc FROM work_workflows WHERE project_id = $1 ORDER BY name`, projectID)
	if err != nil {
		return nil, fmt.Errorf("workflowstore: list: %w", err)
	}
	defer rows.Close()
	out := []workflows.Workflow{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("workflowstore: list: %w", err)
		}
		w, err := decode[workflows.Workflow](raw)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// History implements workflows.Store.
func (s *PG) History(ctx context.Context, id string) ([]workflows.Version, error) {
	var one int
	if err := s.pool.QueryRow(ctx, `SELECT 1 FROM work_workflows WHERE id = $1`, id).Scan(&one); errors.Is(err, pgx.ErrNoRows) {
		return nil, workflows.ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("workflowstore: history: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT version, doc, changed_by, at FROM work_workflow_versions WHERE workflow_id = $1 ORDER BY version DESC`, id)
	if err != nil {
		return nil, fmt.Errorf("workflowstore: history: %w", err)
	}
	defer rows.Close()
	out := []workflows.Version{}
	for rows.Next() {
		var v workflows.Version
		var raw []byte
		if err := rows.Scan(&v.Version, &raw, &v.ChangedBy, &v.At); err != nil {
			return nil, fmt.Errorf("workflowstore: history: %w", err)
		}
		if v.Workflow, err = decode[workflows.Workflow](raw); err != nil {
			return nil, err
		}
		v.At = v.At.UTC()
		out = append(out, v)
	}
	return out, rows.Err()
}

// Delete implements workflows.Store.
func (s *PG) Delete(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM work_workflows WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("workflowstore: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return workflows.ErrNotFound
	}
	return nil
}

// DeleteProject implements workflows.Store.
func (s *PG) DeleteProject(ctx context.Context, projectID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("workflowstore: begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	for _, q := range []string{`DELETE FROM work_workflows WHERE project_id = $1`, `DELETE FROM work_workflow_runs WHERE project_id = $1`} {
		if _, err := tx.Exec(ctx, q, projectID); err != nil {
			return fmt.Errorf("workflowstore: delete project: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// CreateRun implements workflows.Store.
func (s *PG) CreateRun(ctx context.Context, r workflows.Run) (*workflows.Run, error) {
	if r.ProjectID == "" || r.WorkflowID == "" {
		return nil, fmt.Errorf("%w: a run needs a project and a workflow", workflows.ErrInvalid)
	}
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	now := s.stamp()
	r.Version, r.CreatedAt, r.UpdatedAt = 1, now, now
	doc, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("workflowstore: encode run: %w", err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO work_workflow_runs (id, project_id, workflow_id, parent_run_id, state, version, doc, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, 1, $6, $7, $7)`,
		r.ID, r.ProjectID, r.WorkflowID, r.ParentRunID, string(r.State), doc, now)
	if isUnique(err) {
		return nil, workflows.ErrConflict
	}
	if err != nil {
		return nil, fmt.Errorf("workflowstore: create run: %w", err)
	}
	return &r, nil
}

// GetRun implements workflows.Store.
func (s *PG) GetRun(ctx context.Context, id string) (*workflows.Run, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT doc FROM work_workflow_runs WHERE id = $1`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, workflows.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("workflowstore: get run: %w", err)
	}
	r, err := decode[workflows.Run](raw)
	return &r, err
}

// ListRuns implements workflows.Store.
func (s *PG) ListRuns(ctx context.Context, f workflows.RunFilter) ([]workflows.Run, error) {
	states := make([]string, len(f.States))
	for i, st := range f.States {
		states[i] = string(st)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT doc FROM work_workflow_runs
		WHERE ($1 = '' OR project_id = $1) AND ($2 = '' OR workflow_id = $2) AND ($3 = '' OR parent_run_id = $3)
		AND (cardinality($4::text[]) = 0 OR state = ANY($4))
		ORDER BY created_at DESC, id DESC LIMIT $5`, f.ProjectID, f.WorkflowID, f.ParentRunID, states, limit)
	if err != nil {
		return nil, fmt.Errorf("workflowstore: list runs: %w", err)
	}
	defer rows.Close()
	out := []workflows.Run{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("workflowstore: list runs: %w", err)
		}
		r, err := decode[workflows.Run](raw)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateRun implements workflows.Store.
func (s *PG) UpdateRun(ctx context.Context, r workflows.Run) (*workflows.Run, error) {
	now := s.stamp()
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT doc FROM work_workflow_runs WHERE id = $1`, r.ID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, workflows.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("workflowstore: read run: %w", err)
	}
	cur, err := decode[workflows.Run](raw)
	if err != nil {
		return nil, err
	}
	r.ProjectID, r.WorkflowID, r.CreatedAt = cur.ProjectID, cur.WorkflowID, cur.CreatedAt
	expect := r.Version
	r.Version, r.UpdatedAt = expect+1, now
	doc, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("workflowstore: encode run: %w", err)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE work_workflow_runs SET state = $2, version = $3, doc = $4, updated_at = $5 WHERE id = $1 AND version = $6`,
		r.ID, string(r.State), r.Version, doc, now, expect)
	if err != nil {
		return nil, fmt.Errorf("workflowstore: update run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, workflows.ErrStale
	}
	return &r, nil
}
