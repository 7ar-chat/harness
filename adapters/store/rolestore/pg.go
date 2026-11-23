package rolestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/core/roles"
)

// PG is a roles.Store on Postgres (tables of migration 0580, applied by pgstore). Several processes may share it:
// uniqueness is the primary key, and an update runs in one transaction that locks the row.
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

var _ roles.Store = (*PG)(nil)

const pgUniqueViolation = "23505"

func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}

func decode(b []byte) (roles.Role, error) {
	var r roles.Role
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("rolestore: decode role: %w", err)
	}
	return cloneRole(r), nil
}

// Create implements roles.Store.
func (s *PG) Create(ctx context.Context, r roles.Role, actor string) (*roles.Role, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	now := s.now().UTC().Truncate(time.Second)
	r.Version, r.Builtin, r.CreatedBy, r.UpdatedBy, r.CreatedAt, r.UpdatedAt = 1, false, actor, actor, now, now
	doc, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("rolestore: encode role: %w", err)
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO work_roles (scope, scope_id, name, version, doc, created_at, updated_at) VALUES ($1, $2, $3, 1, $4, $5, $5)`,
		string(r.Scope), r.ScopeID, r.Name, doc, now)
	if isUnique(err) {
		return nil, roles.ErrConflict
	}
	if err != nil {
		return nil, fmt.Errorf("rolestore: create role: %w", err)
	}
	return &r, nil
}

// Update implements roles.Store.
func (s *PG) Update(ctx context.Context, r roles.Role, actor string) (*roles.Role, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	now := s.now().UTC().Truncate(time.Second)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("rolestore: begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT doc FROM work_roles WHERE scope = $1 AND scope_id = $2 AND name = $3 FOR UPDATE`,
		string(r.Scope), r.ScopeID, r.Name).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, roles.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("rolestore: read role: %w", err)
	}
	cur, err := decode(raw)
	if err != nil {
		return nil, err
	}
	if r.Version != 0 && r.Version != cur.Version {
		return nil, roles.ErrStale
	}
	if _, err := tx.Exec(ctx, `INSERT INTO work_role_versions (scope, scope_id, name, version, doc, changed_by, at) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		string(r.Scope), r.ScopeID, r.Name, cur.Version, raw, cur.UpdatedBy, cur.UpdatedAt); err != nil {
		return nil, fmt.Errorf("rolestore: keep version: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM work_role_versions WHERE scope = $1 AND scope_id = $2 AND name = $3
		AND version NOT IN (SELECT version FROM work_role_versions WHERE scope = $1 AND scope_id = $2 AND name = $3 ORDER BY version DESC LIMIT $4)`,
		string(r.Scope), r.ScopeID, r.Name, roles.MaxHistory); err != nil {
		return nil, fmt.Errorf("rolestore: trim history: %w", err)
	}
	r.Version, r.Builtin = cur.Version+1, false
	r.CreatedBy, r.CreatedAt, r.UpdatedBy, r.UpdatedAt = cur.CreatedBy, cur.CreatedAt, actor, now
	doc, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("rolestore: encode role: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE work_roles SET version = $4, doc = $5, updated_at = $6 WHERE scope = $1 AND scope_id = $2 AND name = $3`,
		string(r.Scope), r.ScopeID, r.Name, r.Version, doc, now); err != nil {
		return nil, fmt.Errorf("rolestore: update role: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("rolestore: commit: %w", err)
	}
	return &r, nil
}

// Get implements roles.Store.
func (s *PG) Get(ctx context.Context, scope roles.Scope, scopeID, name string) (*roles.Role, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT doc FROM work_roles WHERE scope = $1 AND scope_id = $2 AND name = $3`, string(scope), scopeID, name).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, roles.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("rolestore: get role: %w", err)
	}
	r, err := decode(raw)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// List implements roles.Store.
func (s *PG) List(ctx context.Context, scope roles.Scope, scopeID string) ([]roles.Role, error) {
	rows, err := s.pool.Query(ctx, `SELECT doc FROM work_roles WHERE scope = $1 AND scope_id = $2 ORDER BY name COLLATE "C"`, string(scope), scopeID)
	if err != nil {
		return nil, fmt.Errorf("rolestore: list roles: %w", err)
	}
	defer rows.Close()
	out := []roles.Role{}
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

// History implements roles.Store.
func (s *PG) History(ctx context.Context, scope roles.Scope, scopeID, name string) ([]roles.Version, error) {
	if _, err := s.Get(ctx, scope, scopeID, name); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT version, doc, changed_by, at FROM work_role_versions WHERE scope = $1 AND scope_id = $2 AND name = $3 ORDER BY version DESC`,
		string(scope), scopeID, name)
	if err != nil {
		return nil, fmt.Errorf("rolestore: history: %w", err)
	}
	defer rows.Close()
	out := []roles.Version{}
	for rows.Next() {
		var v roles.Version
		var raw []byte
		if err := rows.Scan(&v.Version, &raw, &v.ChangedBy, &v.At); err != nil {
			return nil, err
		}
		if v.Role, err = decode(raw); err != nil {
			return nil, err
		}
		v.At = v.At.UTC()
		out = append(out, v)
	}
	return out, rows.Err()
}

// Delete implements roles.Store.
func (s *PG) Delete(ctx context.Context, scope roles.Scope, scopeID, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM work_roles WHERE scope = $1 AND scope_id = $2 AND name = $3`, string(scope), scopeID, name)
	if err != nil {
		return fmt.Errorf("rolestore: delete role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return roles.ErrNotFound
	}
	return nil
}

// DeleteScope implements roles.Store.
func (s *PG) DeleteScope(ctx context.Context, scope roles.Scope, scopeID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM work_roles WHERE scope = $1 AND scope_id = $2`, string(scope), scopeID); err != nil {
		return fmt.Errorf("rolestore: delete roles of %s: %w", scopeID, err)
	}
	return nil
}
