package recipestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/core/recipes"
)

// PG is a recipes.Store on Postgres (tables of migration 1100, applied by pgstore). Several processes may share it:
// an update runs in one transaction that locks the row.
type PG struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPG returns a store on pool, whose search_path names the schema the migrations ran in. now (nil: time.Now) is
// the clock.
func NewPG(pool *pgxpool.Pool, now func() time.Time) *PG {
	if now == nil {
		now = time.Now
	}
	return &PG{pool: pool, now: now}
}

var _ recipes.Store = (*PG)(nil)

func decode(b []byte) (recipes.Recipe, error) {
	var r recipes.Recipe
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("recipestore: decode recipe: %w", err)
	}
	return cloneRecipe(r), nil
}

// Create implements recipes.Store.
func (s *PG) Create(ctx context.Context, r recipes.Recipe, actor string) (*recipes.Recipe, error) {
	r, err := prepare(r)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC().Truncate(time.Second)
	r.ID, r.Version, r.Author, r.CreatedAt, r.UpdatedAt = "r_"+uuid.NewString(), 1, actor, now, now
	doc, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("recipestore: encode recipe: %w", err)
	}
	if _, err = s.pool.Exec(ctx, `INSERT INTO work_recipes (id, scope, scope_id, version, doc, created_at, updated_at) VALUES ($1, $2, $3, 1, $4, $5, $5)`,
		r.ID, string(r.Scope), r.ScopeID, doc, now); err != nil {
		return nil, fmt.Errorf("recipestore: create recipe: %w", err)
	}
	return &r, nil
}

// Update implements recipes.Store.
func (s *PG) Update(ctx context.Context, r recipes.Recipe, actor string) (*recipes.Recipe, error) {
	id, version := r.ID, r.Version
	if r.Scope == "" {
		r.Scope = recipes.ScopePersonal
	}
	r, err := prepare(r)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC().Truncate(time.Second)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("recipestore: begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT doc FROM work_recipes WHERE id = $1 FOR UPDATE`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, recipes.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("recipestore: read recipe: %w", err)
	}
	cur, err := decode(raw)
	if err != nil {
		return nil, err
	}
	if version != 0 && version != cur.Version {
		return nil, recipes.ErrStale
	}
	if _, err := tx.Exec(ctx, `INSERT INTO work_recipe_versions (id, version, doc, changed_by, at) VALUES ($1, $2, $3, $4, $5)`,
		id, cur.Version, raw, actor, cur.UpdatedAt); err != nil {
		return nil, fmt.Errorf("recipestore: keep version: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM work_recipe_versions WHERE id = $1
		AND version NOT IN (SELECT version FROM work_recipe_versions WHERE id = $1 ORDER BY version DESC LIMIT $2)`, id, recipes.MaxHistory); err != nil {
		return nil, fmt.Errorf("recipestore: trim history: %w", err)
	}
	r.ID, r.Scope, r.ScopeID, r.Author, r.CreatedAt = id, cur.Scope, cur.ScopeID, cur.Author, cur.CreatedAt
	r.Version, r.UpdatedAt = cur.Version+1, now
	doc, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("recipestore: encode recipe: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE work_recipes SET version = $2, doc = $3, updated_at = $4 WHERE id = $1`, id, r.Version, doc, now); err != nil {
		return nil, fmt.Errorf("recipestore: update recipe: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("recipestore: commit: %w", err)
	}
	return &r, nil
}

// Get implements recipes.Store.
func (s *PG) Get(ctx context.Context, id string) (*recipes.Recipe, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT doc FROM work_recipes WHERE id = $1`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, recipes.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("recipestore: get recipe: %w", err)
	}
	r, err := decode(raw)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// List implements recipes.Store.
func (s *PG) List(ctx context.Context, scope recipes.Scope, scopeID string) ([]recipes.Recipe, error) {
	q, args := `SELECT doc FROM work_recipes ORDER BY doc->>'name' COLLATE "C", id`, []any{}
	if scope != "" {
		q, args = `SELECT doc FROM work_recipes WHERE scope = $1 AND scope_id = $2 ORDER BY doc->>'name' COLLATE "C", id`, []any{string(scope), scopeID}
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("recipestore: list recipes: %w", err)
	}
	defer rows.Close()
	out := []recipes.Recipe{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		r, err := decode(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// History implements recipes.Store.
func (s *PG) History(ctx context.Context, id string) ([]recipes.Version, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT version, doc, changed_by, at FROM work_recipe_versions WHERE id = $1 ORDER BY version DESC`, id)
	if err != nil {
		return nil, fmt.Errorf("recipestore: history: %w", err)
	}
	defer rows.Close()
	out := []recipes.Version{}
	for rows.Next() {
		var v recipes.Version
		var raw []byte
		if err := rows.Scan(&v.Version, &raw, &v.ChangedBy, &v.At); err != nil {
			return nil, err
		}
		if v.Recipe, err = decode(raw); err != nil {
			return nil, err
		}
		v.At = v.At.UTC()
		out = append(out, v)
	}
	return out, rows.Err()
}

// Delete implements recipes.Store.
func (s *PG) Delete(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM work_recipes WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("recipestore: delete recipe: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return recipes.ErrNotFound
	}
	_, _ = s.pool.Exec(ctx, `DELETE FROM work_recipe_uses WHERE id = $1`, id)
	return nil
}

// DeleteScope implements recipes.Store.
func (s *PG) DeleteScope(ctx context.Context, scope recipes.Scope, scopeID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM work_recipe_uses WHERE id IN (SELECT id FROM work_recipes WHERE scope = $1 AND scope_id = $2)`, string(scope), scopeID); err != nil {
		return fmt.Errorf("recipestore: delete uses of %s: %w", scopeID, err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM work_recipes WHERE scope = $1 AND scope_id = $2`, string(scope), scopeID); err != nil {
		return fmt.Errorf("recipestore: delete recipes of %s: %w", scopeID, err)
	}
	return nil
}

// AddUse implements recipes.Store.
func (s *PG) AddUse(ctx context.Context, id string) error {
	if _, err := s.pool.Exec(ctx, `INSERT INTO work_recipe_uses (id, uses) VALUES ($1, 1) ON CONFLICT (id) DO UPDATE SET uses = work_recipe_uses.uses + 1`, id); err != nil {
		return fmt.Errorf("recipestore: count use: %w", err)
	}
	return nil
}

// Uses implements recipes.Store.
func (s *PG) Uses(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, uses FROM work_recipe_uses`)
	if err != nil {
		return nil, fmt.Errorf("recipestore: uses: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}
