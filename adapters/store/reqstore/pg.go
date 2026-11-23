package reqstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/core/requirements"
)

// PG is a requirements.Store on Postgres (tables of migration 0590, applied by pgstore). Several processes may
// share it: the import dedupe is a unique index, an update is a locked read-modify-write.
type PG struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

var _ requirements.Store = (*PG)(nil)

// NewPG returns a store on pool, whose search_path names the schema the migrations ran in.
func NewPG(pool *pgxpool.Pool, now func() time.Time) *PG {
	if now == nil {
		now = time.Now
	}
	return &PG{pool: pool, now: now}
}

const cols = `id, project_id, title, body, status, priority, labels, source, author, thread_ids, task_ids, pr, flag, created_at, updated_at`

func scan(row pgx.Row) (requirements.Requirement, error) {
	var r requirements.Requirement
	var status string
	var labels, source, threads, tasks, pr []byte
	if err := row.Scan(&r.ID, &r.ProjectID, &r.Title, &r.Body, &status, &r.Priority, &labels, &source, &r.Author,
		&threads, &tasks, &pr, &r.Flag, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return r, err
	}
	r.Status = requirements.Status(status)
	for _, f := range []struct {
		b []byte
		v any
	}{{labels, &r.Labels}, {source, &r.Source}, {threads, &r.ThreadIDs}, {tasks, &r.TaskIDs}, {pr, &r.PR}} {
		if err := json.Unmarshal(f.b, f.v); err != nil {
			return r, fmt.Errorf("decode requirement %s: %w", r.ID, err)
		}
	}
	return requirements.Clone(r), nil
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return requirements.ErrNotFound
	}
	return err
}

func js(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func (s *PG) Create(ctx context.Context, r requirements.Requirement) (*requirements.Requirement, bool, error) {
	r, err := requirements.Prepare(r)
	if err != nil {
		return nil, false, err
	}
	now := s.now()
	r.ID, r.CreatedAt, r.UpdatedAt = uuid.NewString(), now, now
	tag, err := s.pool.Exec(ctx, `INSERT INTO work_requirements (`+cols+`, source_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) ON CONFLICT DO NOTHING`,
		r.ID, r.ProjectID, r.Title, r.Body, string(r.Status), r.Priority, js(orEmpty(r.Labels)), js(r.Source), r.Author,
		js(orEmpty(r.ThreadIDs)), js(orEmpty(r.TaskIDs)), js(r.PR), r.Flag, r.CreatedAt, r.UpdatedAt, r.Source.Key())
	if err != nil {
		return nil, false, fmt.Errorf("create requirement: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return &r, true, nil
	}
	// The source key exists already: hand back that requirement.
	cur, err := scan(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM work_requirements WHERE project_id = $1 AND source_key = $2`,
		r.ProjectID, r.Source.Key()))
	if err != nil {
		return nil, false, notFound(err)
	}
	return &cur, false, nil
}

func orEmpty(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

func (s *PG) Get(ctx context.Context, id string) (*requirements.Requirement, error) {
	r, err := scan(s.pool.QueryRow(ctx, `SELECT `+cols+` FROM work_requirements WHERE id = $1`, id))
	if err != nil {
		return nil, notFound(err)
	}
	return &r, nil
}

func (s *PG) Update(ctx context.Context, id string, p requirements.Patch) (*requirements.Requirement, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	r, err := scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM work_requirements WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return nil, notFound(err)
	}
	if err := requirements.Apply(&r, p); err != nil {
		return nil, err
	}
	r.UpdatedAt = s.now()
	if _, err := tx.Exec(ctx, `UPDATE work_requirements SET title=$2, body=$3, status=$4, priority=$5, labels=$6, thread_ids=$7,
		task_ids=$8, pr=$9, flag=$10, updated_at=$11 WHERE id = $1`,
		id, r.Title, r.Body, string(r.Status), r.Priority, js(orEmpty(r.Labels)), js(orEmpty(r.ThreadIDs)), js(orEmpty(r.TaskIDs)),
		js(r.PR), r.Flag, r.UpdatedAt); err != nil {
		return nil, fmt.Errorf("update requirement: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &r, nil
}

func (s *PG) Delete(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM work_requirements WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete requirement: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return requirements.ErrNotFound
	}
	return nil
}

func (s *PG) List(ctx context.Context, f requirements.Filter) ([]requirements.Requirement, error) {
	q := `SELECT ` + cols + ` FROM work_requirements WHERE true`
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		q += fmt.Sprintf(" AND "+cond, len(args))
	}
	if f.ProjectID != "" {
		add("project_id = $%d", f.ProjectID)
	}
	if len(f.Statuses) > 0 {
		st := make([]string, len(f.Statuses))
		for i, v := range f.Statuses {
			st[i] = string(v)
		}
		add("status = ANY($%d)", st)
	}
	if f.ThreadID != "" {
		add("thread_ids @> to_jsonb($%d::text)", f.ThreadID)
	}
	if f.TaskID != "" {
		add("task_ids @> to_jsonb($%d::text)", f.TaskID)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list requirements: %w", err)
	}
	defer rows.Close()
	out := []requirements.Requirement{}
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		if f.Match(r) { // label and text search run here: the lists are small
			out = append(out, r)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b requirements.Requirement) int {
		switch {
		case requirements.Less(a, b):
			return -1
		case requirements.Less(b, a):
			return 1
		}
		return 0
	})
	limit := f.Limit
	if limit <= 0 {
		limit = 500
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *PG) GetSettings(ctx context.Context, projectID string) (requirements.Settings, error) {
	var b []byte
	err := s.pool.QueryRow(ctx, `SELECT settings FROM work_requirement_settings WHERE project_id = $1`, projectID).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return requirements.Settings{ProjectID: projectID}, nil
	}
	if err != nil {
		return requirements.Settings{}, err
	}
	var st requirements.Settings
	if err := json.Unmarshal(b, &st); err != nil {
		return requirements.Settings{}, err
	}
	st.ProjectID = projectID
	return st, nil
}

func (s *PG) PutSettings(ctx context.Context, st requirements.Settings) error {
	if st.ProjectID == "" {
		return errors.Join(requirements.ErrInvalid, errors.New("settings belong to a project"))
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO work_requirement_settings (project_id, settings) VALUES ($1, $2)
		ON CONFLICT (project_id) DO UPDATE SET settings = EXCLUDED.settings`, st.ProjectID, js(st))
	return err
}
