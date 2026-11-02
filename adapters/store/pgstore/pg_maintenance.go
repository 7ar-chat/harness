package pgstore

import (
	"context"
	"errors"
	"fmt"
	"github.com/7ar-chat/harness/core/store"
	"time"

	"github.com/jackc/pgx/v5"
)

const lockPoll = 25 * time.Millisecond

// AdvisoryLock implements ArtifactStore with a session-level Postgres advisory
// lock held on a dedicated pool connection. pg_try_advisory_lock is polled so
// a cancelled wait can never leave a lock behind; at most half of the pool's
// connections hold locks at a time (the holders still need connections for
// their own queries).
func (s *PG) AdvisoryLock(ctx context.Context, name string, wait time.Duration) (func(), error) {
	deadline := time.Now().Add(wait)
	wctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	select {
	case s.lockSlots <- struct{}{}:
	case <-wctx.Done():
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, store.ErrLockTimeout
	}
	conn, err := s.pool.Acquire(wctx)
	if err != nil {
		<-s.lockSlots
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return nil, store.ErrLockTimeout
		}
		return nil, err
	}
	fail := func(err error) (func(), error) {
		conn.Release()
		<-s.lockSlots
		return nil, err
	}
	for {
		var ok bool
		if err := conn.QueryRow(wctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, name).Scan(&ok); err != nil {
			if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
				return fail(store.ErrLockTimeout)
			}
			return fail(fmt.Errorf("advisory lock %s: %w", name, err))
		}
		if ok {
			break
		}
		select {
		case <-wctx.Done():
			if ctx.Err() != nil {
				return fail(ctx.Err())
			}
			return fail(store.ErrLockTimeout)
		case <-time.After(lockPoll):
		}
	}
	return func() {
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var released bool
		if err := conn.QueryRow(uctx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, name).Scan(&released); err != nil || !released {
			// Never hand a connection that may still hold the lock back to the pool.
			_ = conn.Conn().Close(uctx)
		}
		conn.Release()
		<-s.lockSlots
	}, nil
}

// PurgeDeletedThreads implements ArtifactStore.
func (s *PG) PurgeDeletedThreads(ctx context.Context, olderThan time.Duration, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT id::text FROM work_threads
		WHERE deleted_at IS NOT NULL AND deleted_at < now() - make_interval(secs => $1)
		ORDER BY deleted_at LIMIT $2 FOR UPDATE SKIP LOCKED`, olderThan.Seconds(), limit)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	// The content blobs lose these references now: their grace period starts.
	if _, err := tx.Exec(ctx, `INSERT INTO work_blob_gc (key, last_ref_at)
		SELECT DISTINCT v.sha256, now() FROM work_artifact_versions v JOIN work_artifacts a ON a.id = v.artifact_id
		WHERE a.thread_id = ANY($1::uuid[])
		ON CONFLICT (key) DO UPDATE SET last_ref_at = now()`, ids); err != nil {
		return 0, err
	}
	// Runs, messages, events, artifacts, versions, goals and feedback cascade.
	if _, err := tx.Exec(ctx, `DELETE FROM work_threads WHERE id = ANY($1::uuid[])`, ids); err != nil {
		return 0, err
	}
	return len(ids), tx.Commit(ctx)
}

// unreferencedSQL is the collection condition on a work_blob_gc row g: no
// artifact version and no current workspace Git archive references the blob
// and its last reference is older than the grace period ($2, seconds). Blobs
// are queued before they are stored (C-25), so a referenced key may be queued.
const unreferencedSQL = `g.last_ref_at < now() - make_interval(secs => $2)
		AND NOT EXISTS (SELECT 1 FROM work_artifact_versions v WHERE v.sha256 = g.key)
		AND NOT EXISTS (SELECT 1 FROM work_workspace_git w WHERE w.blob_key = g.key)`

// TouchBlob implements ArtifactStore (the row lock of an in-flight
// CollectBlob / CollectBlobs makes it wait for the collection to commit).
func (s *PG) TouchBlob(ctx context.Context, key string) error {
	_, err := s.pool.Exec(ctx, `UPDATE work_blob_gc SET last_ref_at = now() WHERE key = $1`, key)
	return err
}

// BlobCandidates implements ArtifactStore.
func (s *PG) BlobCandidates(ctx context.Context, grace time.Duration, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 500
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM work_blob_gc g
		WHERE g.last_ref_at < now() - make_interval(secs => $1)
		AND (EXISTS (SELECT 1 FROM work_artifact_versions v WHERE v.sha256 = g.key)
			OR EXISTS (SELECT 1 FROM work_workspace_git w WHERE w.blob_key = g.key))`, grace.Seconds()); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT key FROM work_blob_gc g WHERE `+unreferencedSQL+`
		ORDER BY g.last_ref_at, g.key LIMIT $1`, limit, grace.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// CollectBlob implements ArtifactStore: the conditional DELETE of the queue
// row and del run in one transaction, committed only once del succeeded.
func (s *PG) CollectBlob(ctx context.Context, key string, grace time.Duration, del func(context.Context) error) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var got string
	err = tx.QueryRow(ctx, `DELETE FROM work_blob_gc g WHERE g.key = $1 AND `+unreferencedSQL+` RETURNING g.key`,
		key, grace.Seconds()).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := del(ctx); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// CollectBlobs implements BlobStore: one statement removes the queue rows
// still meeting the condition and their content and preview from work_blobs.
func (s *PG) CollectBlobs(ctx context.Context, keys []string, grace time.Duration) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `WITH claimed AS (
			DELETE FROM work_blob_gc g WHERE g.key = ANY($1::text[]) AND `+unreferencedSQL+` RETURNING g.key
		), gone AS (
			DELETE FROM work_blobs b USING claimed c WHERE b.key = c.key OR b.key = 'preview/' || c.key || '.pdf'
		)
		SELECT key FROM claimed ORDER BY key`, keys, grace.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
