package executorstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/core/executors"
)

// PG is an executors.Store on Postgres (table of migration 0900, applied by pgstore). Several processes may share
// it: uniqueness is the primary key.
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

var _ executors.Store = (*PG)(nil)

func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func decode(b []byte) (executors.Executor, error) {
	var e executors.Executor
	if err := json.Unmarshal(b, &e); err != nil {
		return e, fmt.Errorf("executorstore: decode: %w", err)
	}
	return e, nil
}

// Create implements executors.Store.
func (s *PG) Create(ctx context.Context, e executors.Executor, actor string) (*executors.Executor, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM work_executors WHERE project_id = $1`, e.ProjectID).Scan(&n); err != nil {
		return nil, fmt.Errorf("executorstore: count: %w", err)
	}
	if n >= executors.MaxListed {
		return nil, fmt.Errorf("%w: too many executors in the project", executors.ErrInvalid)
	}
	now := s.now().UTC().Truncate(time.Second)
	e.CreatedBy, e.CreatedAt, e.UpdatedAt = actor, now, now
	doc, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("executorstore: encode: %w", err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO work_executors (project_id, name, doc, created_at, updated_at) VALUES ($1, $2, $3, $4, $4)`,
		e.ProjectID, e.Name, doc, now)
	if isUnique(err) {
		return nil, executors.ErrConflict
	}
	if err != nil {
		return nil, fmt.Errorf("executorstore: create: %w", err)
	}
	return &e, nil
}

// Update implements executors.Store.
func (s *PG) Update(ctx context.Context, e executors.Executor, _ string) (*executors.Executor, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	cur, err := s.Get(ctx, e.ProjectID, e.Name)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC().Truncate(time.Second)
	e.CreatedBy, e.CreatedAt, e.UpdatedAt = cur.CreatedBy, cur.CreatedAt, now
	doc, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("executorstore: encode: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE work_executors SET doc = $3, updated_at = $4 WHERE project_id = $1 AND name = $2`,
		e.ProjectID, e.Name, doc, now); err != nil {
		return nil, fmt.Errorf("executorstore: update: %w", err)
	}
	return &e, nil
}

// Get implements executors.Store.
func (s *PG) Get(ctx context.Context, projectID, name string) (*executors.Executor, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT doc FROM work_executors WHERE project_id = $1 AND name = $2`, projectID, name).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, executors.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("executorstore: get: %w", err)
	}
	e, err := decode(raw)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// List implements executors.Store.
func (s *PG) List(ctx context.Context, projectID string) ([]executors.Executor, error) {
	rows, err := s.pool.Query(ctx, `SELECT doc FROM work_executors WHERE project_id = $1 ORDER BY name COLLATE "C"`, projectID)
	if err != nil {
		return nil, fmt.Errorf("executorstore: list: %w", err)
	}
	defer rows.Close()
	out := []executors.Executor{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		e, err := decode(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Delete implements executors.Store.
func (s *PG) Delete(ctx context.Context, projectID, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM work_executors WHERE project_id = $1 AND name = $2`, projectID, name)
	if err != nil {
		return fmt.Errorf("executorstore: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return executors.ErrNotFound
	}
	return nil
}

// DeleteProject implements executors.Store.
func (s *PG) DeleteProject(ctx context.Context, projectID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM work_executors WHERE project_id = $1`, projectID); err != nil {
		return fmt.Errorf("executorstore: delete executors of %s: %w", projectID, err)
	}
	return nil
}
