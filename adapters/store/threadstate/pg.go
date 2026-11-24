package threadstate

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/core/store"
)

// PG is a store.ThreadStateStore on Postgres (table work_thread_state of migration 0700). An update runs in one
// transaction holding an advisory lock on the (thread, key), so replicas serialise.
type PG struct{ pool *pgxpool.Pool }

var _ store.ThreadStateStore = (*PG)(nil)

// NewPG returns a store on pool, whose search_path names the schema the migrations ran in.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

// GetThreadState implements store.ThreadStateStore.
func (s *PG) GetThreadState(ctx context.Context, threadID, key string) (json.RawMessage, error) {
	var doc []byte
	err := s.pool.QueryRow(ctx, `SELECT doc FROM work_thread_state WHERE thread_id=$1 AND key=$2`, threadID, key).Scan(&doc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return doc, err
}

// UpdateThreadState implements store.ThreadStateStore.
func (s *PG) UpdateThreadState(ctx context.Context, threadID, key string, fn func(json.RawMessage) (json.RawMessage, error)) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The row may not exist yet: an advisory lock on (thread, key) serialises the first writers too.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, threadID+"\x1f"+key); err != nil {
		return err
	}
	var old []byte
	err = tx.QueryRow(ctx, `SELECT doc FROM work_thread_state WHERE thread_id=$1 AND key=$2`, threadID, key).Scan(&old)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	out, err := fn(old)
	if err != nil {
		return err
	}
	if out == nil {
		_, err = tx.Exec(ctx, `DELETE FROM work_thread_state WHERE thread_id=$1 AND key=$2`, threadID, key)
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO work_thread_state (thread_id, key, doc, updated_at) VALUES ($1,$2,$3,now())
			ON CONFLICT (thread_id, key) DO UPDATE SET doc=EXCLUDED.doc, updated_at=now()`, threadID, key, []byte(out))
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DeleteThreadState implements store.ThreadStateStore.
func (s *PG) DeleteThreadState(ctx context.Context, threadID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM work_thread_state WHERE thread_id=$1`, threadID)
	return err
}
