package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/7ar-chat/harness/core/store"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const memoryEntryCols = `id, user_login, path, title, content, tags, importance, metadata, scope, repository, source, status,
	coalesce(source_thread_id::text, ''), content_hash, created_at, updated_at, content_updated_at, deleted_at, version, proposal`

func scanMemoryEntry(row pgx.Row) (*store.MemoryEntry, error) {
	var e store.MemoryEntry
	var tags, meta, proposal []byte
	if err := row.Scan(&e.ID, &e.UserLogin, &e.Path, &e.Title, &e.Content, &tags, &e.Importance, &meta, &e.Scope, &e.Repository,
		&e.Source, &e.Status, &e.SourceThreadID, &e.ContentHash, &e.CreatedAt, &e.UpdatedAt, &e.ContentUpdatedAt, &e.DeletedAt,
		&e.Version, &proposal); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal(tags, &e.Tags); err != nil || e.Tags == nil {
		e.Tags = []string{}
	}
	if err := json.Unmarshal(meta, &e.Metadata); err != nil || e.Metadata == nil {
		e.Metadata = map[string]any{}
	}
	if len(proposal) > 0 && string(proposal) != "null" {
		var p store.MemoryProposal
		if err := json.Unmarshal(proposal, &p); err == nil {
			if p.Tags == nil {
				p.Tags = []string{}
			}
			if p.Metadata == nil {
				p.Metadata = map[string]any{}
			}
			e.Proposal = &p
		}
	}
	return &e, nil
}

func scanMemoryEntries(rows pgx.Rows) ([]*store.MemoryEntry, error) {
	defer rows.Close()
	var out []*store.MemoryEntry
	for rows.Next() {
		e, err := scanMemoryEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func memoryJSON(e *store.MemoryEntry) (tags, meta []byte) {
	t := e.Tags
	if t == nil {
		t = []string{}
	}
	m := e.Metadata
	if m == nil {
		m = map[string]any{}
	}
	tags, _ = json.Marshal(store.CleanValue(anyStrings(t)))
	meta, _ = json.Marshal(store.CleanValue(m))
	return store.CleanJSON(tags), store.CleanJSON(meta)
}

// proposalJSON is the proposal column (NULL without a proposal).
func proposalJSON(p *store.MemoryProposal) []byte {
	if p == nil {
		return nil
	}
	cp := p.Clone()
	cp.Path, cp.Title, cp.Content = store.CleanText(cp.Path), store.CleanText(cp.Title), store.CleanText(cp.Content)
	if cp.Tags == nil {
		cp.Tags = []string{}
	}
	b, _ := json.Marshal(cp)
	var v any
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	out, _ := json.Marshal(store.CleanValue(v))
	return store.CleanJSON(out)
}

func anyStrings(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

func memoryPathClash(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "work_memory_entries_path_uidx" {
		return store.ErrMemoryPathExists
	}
	return err
}

func (s *PG) MemoryEnabled(ctx context.Context, login string) (bool, error) {
	enabled := true
	err := s.pool.QueryRow(ctx, `SELECT memory_enabled FROM work_user_settings WHERE user_login = $1`, login).Scan(&enabled)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	return enabled, nil
}

func (s *PG) SetMemoryEnabled(ctx context.Context, login string, enabled bool) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO work_user_settings (user_login, memory_enabled) VALUES ($1, $2)
		ON CONFLICT (user_login) DO UPDATE SET memory_enabled = excluded.memory_enabled, updated_at = now()`, login, enabled)
	return err
}

func (s *PG) ListMemoryEntries(ctx context.Context, login string, statuses ...string) ([]*store.MemoryEntry, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+memoryEntryCols+` FROM work_memory_entries
		WHERE user_login = $1 AND (coalesce(cardinality($2::text[]), 0) = 0 OR status = ANY($2)) ORDER BY created_at, id`, login, statuses)
	if err != nil {
		return nil, err
	}
	return scanMemoryEntries(rows)
}

func (s *PG) GetMemoryEntry(ctx context.Context, login, id string) (*store.MemoryEntry, error) {
	return scanMemoryEntry(s.pool.QueryRow(ctx, `SELECT `+memoryEntryCols+` FROM work_memory_entries WHERE user_login = $1 AND id = $2`,
		login, id))
}

func (s *PG) GetMemoryEntryByPath(ctx context.Context, login, path string) (*store.MemoryEntry, error) {
	return scanMemoryEntry(s.pool.QueryRow(ctx, `SELECT `+memoryEntryCols+` FROM work_memory_entries WHERE user_login = $1 AND path = $2
		ORDER BY (status <> 'deleted') DESC, updated_at DESC LIMIT 1`, login, path))
}

// lockMemoryUser serializes the per-user count checks (create, status changes).
func lockMemoryUser(ctx context.Context, tx pgx.Tx, login string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('work_memory_entries:' || $1, 0))`, login)
	return err
}

// checkMemoryLimit fails with ErrMemoryLimit when the user has max entries of status (≤ 0: no limit).
func checkMemoryLimit(ctx context.Context, tx pgx.Tx, login, status string, max int) error {
	if max <= 0 {
		return nil
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM work_memory_entries WHERE user_login = $1 AND status = $2`, login, status).Scan(&n); err != nil {
		return err
	}
	if n >= max {
		return store.ErrMemoryLimit
	}
	return nil
}

func (s *PG) CreateMemoryEntry(ctx context.Context, e *store.MemoryEntry, limits store.MemoryLimits) (*store.MemoryEntry, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockMemoryUser(ctx, tx, e.UserLogin); err != nil {
		return nil, err
	}
	if err := checkMemoryLimit(ctx, tx, e.UserLogin, e.Status, limits.Of(e.Status)); err != nil {
		return nil, err
	}
	var source *string
	if validUUID(e.SourceThreadID) {
		source = &e.SourceThreadID
	}
	tags, meta := memoryJSON(e)
	now := e.CreatedAt
	if now.IsZero() {
		now = time.Now()
	}
	out, err := scanMemoryEntry(tx.QueryRow(ctx, `INSERT INTO work_memory_entries (id, user_login, path, title, content, tags, importance,
			metadata, scope, repository, source, status, source_thread_id, content_hash, created_at, updated_at, content_updated_at, deleted_at,
			proposal)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $15, $15, NULL, $16) RETURNING `+memoryEntryCols,
		e.ID, e.UserLogin, store.CleanText(e.Path), store.CleanText(e.Title), store.CleanText(e.Content), tags, e.Importance, meta, e.Scope, e.Repository,
		e.Source, e.Status, source, e.ContentHash, now, proposalJSON(e.Proposal)))
	if err != nil {
		return nil, memoryPathClash(err)
	}
	return out, tx.Commit(ctx)
}

func (s *PG) UpdateMemoryEntry(ctx context.Context, e *store.MemoryEntry, limits store.MemoryLimits) (*store.MemoryEntry, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockMemoryUser(ctx, tx, e.UserLogin); err != nil {
		return nil, err
	}
	var status string
	var version int64
	err = tx.QueryRow(ctx, `SELECT status, version FROM work_memory_entries WHERE user_login = $1 AND id = $2 FOR UPDATE`,
		e.UserLogin, e.ID).Scan(&status, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if version != e.Version {
		return nil, store.ErrMemoryConflict
	}
	if e.Status != status {
		if err := checkMemoryLimit(ctx, tx, e.UserLogin, e.Status, limits.Of(e.Status)); err != nil {
			return nil, err
		}
	}
	tags, meta := memoryJSON(e)
	out, err := scanMemoryEntry(tx.QueryRow(ctx, `UPDATE work_memory_entries SET path = $3, title = $4, content = $5, tags = $6,
			importance = $7, metadata = $8, status = $9, content_hash = $10, updated_at = $11, content_updated_at = $12, deleted_at = $13,
			proposal = $15, version = version + 1
		WHERE user_login = $1 AND id = $2 AND version = $14 RETURNING `+memoryEntryCols,
		e.UserLogin, e.ID, store.CleanText(e.Path), store.CleanText(e.Title), store.CleanText(e.Content), tags, e.Importance, meta, e.Status,
		e.ContentHash, e.UpdatedAt, e.ContentUpdatedAt, e.DeletedAt, e.Version, proposalJSON(e.Proposal)))
	if errors.Is(err, store.ErrNotFound) {
		return nil, store.ErrMemoryConflict
	}
	if err != nil {
		return nil, memoryPathClash(err)
	}
	return out, tx.Commit(ctx)
}

// memoryMissOrConflict tells why a guarded write matched no row.
func (s *PG) memoryMissOrConflict(ctx context.Context, login, id string) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM work_memory_entries WHERE user_login = $1 AND id = $2)`,
		login, id).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return store.ErrMemoryConflict
	}
	return store.ErrNotFound
}

func (s *PG) DeleteMemoryEntry(ctx context.Context, login, id, expectStatus string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM work_memory_entries WHERE user_login = $1 AND id = $2 AND ($3 = '' OR status = $3)`,
		login, id, expectStatus)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if expectStatus == "" {
			return store.ErrNotFound
		}
		return s.memoryMissOrConflict(ctx, login, id)
	}
	return nil
}

func (s *PG) ListMemoryUsers(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT user_login FROM work_memory_entries WHERE status = 'active' ORDER BY user_login`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var login string
		if err := rows.Scan(&login); err != nil {
			return nil, err
		}
		out = append(out, login)
	}
	return out, rows.Err()
}

func (s *PG) PurgeDeletedMemoryEntries(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM work_memory_entries WHERE status = 'deleted' AND deleted_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
