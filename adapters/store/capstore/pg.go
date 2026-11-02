package capstore

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

	"github.com/7ar-chat/harness/core/capabilities"
)

// PG is a capabilities.Store on Postgres (tables of migration 0500, applied by pgstore). Several processes
// may share it: uniqueness is enforced by indexes, never by a read followed by a write.
type PG struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewPG returns a store on pool, whose search_path names the schema the migrations ran in. now (nil:
// time.Now) is the clock.
func NewPG(pool *pgxpool.Pool, now func() time.Time) *PG {
	if now == nil {
		now = time.Now
	}
	return &PG{pool: pool, now: now}
}

var _ capabilities.Store = (*PG)(nil)

const pgUniqueViolation = "23505"

func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return capabilities.ErrNotFound
	}
	return err
}

// SeedCatalog implements capabilities.Store. The whole seeding is one transaction under an advisory lock:
// replicas starting together take turns, and none sees a catalog half way between two versions.
func (s *PG) SeedCatalog(ctx context.Context, entries []capabilities.Entry) error {
	want, err := seedSet(entries)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('work_mcp_catalog', 0))`); err != nil {
		return fmt.Errorf("lock catalog: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM work_mcp_catalog WHERE builtin AND id <> ALL($1)`, ids); err != nil {
		return fmt.Errorf("seed catalog: %w", err)
	}
	now := s.now()
	for _, id := range ids {
		e := want[id]
		e.Builtin = false // the column is the source of truth
		b, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("encode catalog entry %q: %w", id, err)
		}
		// An entry added in another way (not builtin) keeps its content; an unchanged one keeps updated_at.
		if _, err := tx.Exec(ctx, `INSERT INTO work_mcp_catalog (id, entry, builtin, updated_at) VALUES ($1, $2, true, $3)
			ON CONFLICT (id) DO UPDATE SET entry = EXCLUDED.entry, updated_at = EXCLUDED.updated_at
			WHERE work_mcp_catalog.builtin AND work_mcp_catalog.entry IS DISTINCT FROM EXCLUDED.entry`, id, b, now); err != nil {
			return fmt.Errorf("seed catalog entry %q: %w", id, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func scanEntry(r pgx.Row) (*capabilities.Entry, error) {
	var b []byte
	var builtin bool
	var id string
	if err := r.Scan(&id, &b, &builtin); err != nil {
		return nil, err
	}
	var e capabilities.Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("decode catalog entry %q: %w", id, err)
	}
	e.ID, e.Builtin = id, builtin
	e = cloneEntry(e)
	return &e, nil
}

// ListCatalog implements capabilities.Store, ordered by id.
func (s *PG) ListCatalog(ctx context.Context) ([]capabilities.Entry, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, entry, builtin FROM work_mcp_catalog ORDER BY id COLLATE "C"`)
	if err != nil {
		return nil, fmt.Errorf("list catalog: %w", err)
	}
	defer rows.Close()
	out := []capabilities.Entry{}
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// GetCatalogEntry implements capabilities.Store.
func (s *PG) GetCatalogEntry(ctx context.Context, id string) (*capabilities.Entry, error) {
	e, err := scanEntry(s.pool.QueryRow(ctx, `SELECT id, entry, builtin FROM work_mcp_catalog WHERE id = $1`, id))
	return e, notFound(err)
}

const installCols = `id, scope, scope_id, catalog_id, name, url, transport, tools, enabled, secrets, secret_keys, created_by, created_at, updated_at`

func scanInstall(r pgx.Row) (*capabilities.Install, error) {
	var in capabilities.Install
	var scope string
	var tools, keys []byte
	if err := r.Scan(&in.ID, &scope, &in.ScopeID, &in.CatalogID, &in.Name, &in.URL, &in.Transport, &tools, &in.Enabled,
		&in.Secrets, &keys, &in.CreatedBy, &in.CreatedAt, &in.UpdatedAt); err != nil {
		return nil, err
	}
	in.Scope = capabilities.Scope(scope)
	if err := json.Unmarshal(tools, &in.Tools); err != nil {
		return nil, fmt.Errorf("decode tools of install %q: %w", in.ID, err)
	}
	if err := json.Unmarshal(keys, &in.SecretKeys); err != nil {
		return nil, fmt.Errorf("decode secret keys of install %q: %w", in.ID, err)
	}
	in = cloneInstall(in)
	return &in, nil
}

// PutInstall implements capabilities.Store.
func (s *PG) PutInstall(ctx context.Context, in capabilities.Install) (*capabilities.Install, error) {
	in, err := prepare(in)
	if err != nil {
		return nil, err
	}
	tools, err := json.Marshal(append([]string{}, in.Tools...))
	if err != nil {
		return nil, fmt.Errorf("encode tools: %w", err)
	}
	keys, err := json.Marshal(append([]string{}, in.SecretKeys...))
	if err != nil {
		return nil, fmt.Errorf("encode secret keys: %w", err)
	}
	secrets := in.Secrets
	if secrets == nil {
		secrets = []byte{}
	}
	now := s.now()
	conflict := fmt.Errorf("%w: %q", capabilities.ErrConflict, in.Name)

	if in.CatalogID == "" {
		var taken bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_mcp_catalog WHERE id = $1)`, in.Name).Scan(&taken); err != nil {
			return nil, fmt.Errorf("check catalog: %w", err)
		}
		if taken {
			return nil, fmt.Errorf("%w: %q is a catalog entry", capabilities.ErrConflict, in.Name)
		}
	}

	var row pgx.Row
	switch {
	case in.ID != "":
		row = s.pool.QueryRow(ctx, `UPDATE work_mcp_installs SET name = $5, url = $6, transport = $7, tools = $8, enabled = $9,
				secrets = $10, secret_keys = $11, updated_at = $12
			WHERE id = $1 AND scope = $2 AND scope_id = $3 AND catalog_id = $4 RETURNING `+installCols,
			in.ID, string(in.Scope), in.ScopeID, in.CatalogID, in.Name, in.URL, in.Transport, tools, in.Enabled, secrets, keys, now)
	case in.CatalogID != "":
		// Many callers may race to create the same install: the unique index decides, the losers update it.
		row = s.pool.QueryRow(ctx, `INSERT INTO work_mcp_installs (`+installCols+`)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$13)
			ON CONFLICT (scope, scope_id, catalog_id) WHERE catalog_id <> '' DO UPDATE SET name = EXCLUDED.name, url = EXCLUDED.url,
				transport = EXCLUDED.transport, tools = EXCLUDED.tools, enabled = EXCLUDED.enabled, secrets = EXCLUDED.secrets,
				secret_keys = EXCLUDED.secret_keys, updated_at = EXCLUDED.updated_at
			RETURNING `+installCols,
			uuid.NewString(), string(in.Scope), in.ScopeID, in.CatalogID, in.Name, in.URL, in.Transport, tools, in.Enabled, secrets, keys,
			in.CreatedBy, now)
	default:
		row = s.pool.QueryRow(ctx, `INSERT INTO work_mcp_installs (`+installCols+`)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$13) RETURNING `+installCols,
			uuid.NewString(), string(in.Scope), in.ScopeID, in.CatalogID, in.Name, in.URL, in.Transport, tools, in.Enabled, secrets, keys,
			in.CreatedBy, now)
	}
	out, err := scanInstall(row)
	switch {
	case isUnique(err):
		return nil, conflict
	case err != nil:
		return nil, fmt.Errorf("put install: %w", notFound(err))
	}
	return out, nil
}

// GetInstall implements capabilities.Store.
func (s *PG) GetInstall(ctx context.Context, id string) (*capabilities.Install, error) {
	in, err := scanInstall(s.pool.QueryRow(ctx, `SELECT `+installCols+` FROM work_mcp_installs WHERE id = $1`, id))
	return in, notFound(err)
}

// ListInstalls implements capabilities.Store, ordered by name.
func (s *PG) ListInstalls(ctx context.Context, scope capabilities.Scope, scopeID string) ([]capabilities.Install, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+installCols+` FROM work_mcp_installs WHERE scope = $1 AND scope_id = $2 ORDER BY name COLLATE "C"`,
		string(scope), scopeID)
	if err != nil {
		return nil, fmt.Errorf("list installs: %w", err)
	}
	defer rows.Close()
	var out []capabilities.Install
	for rows.Next() {
		in, err := scanInstall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *in)
	}
	return out, rows.Err()
}

// DeleteInstall implements capabilities.Store.
func (s *PG) DeleteInstall(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM work_mcp_installs WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete install: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return capabilities.ErrNotFound
	}
	return nil
}

// DeleteInstalls implements capabilities.Store.
func (s *PG) DeleteInstalls(ctx context.Context, scope capabilities.Scope, scopeID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM work_mcp_installs WHERE scope = $1 AND scope_id = $2`, string(scope), scopeID); err != nil {
		return fmt.Errorf("delete installs: %w", err)
	}
	return nil
}
