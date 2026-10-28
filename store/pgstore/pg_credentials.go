package pgstore

import (
	"context"
	"errors"
	"github.com/GhaziBenDahmane/harness/store"

	"github.com/jackc/pgx/v5"
)

// PutRunCredential implements CredentialStore.
func (s *PG) PutRunCredential(ctx context.Context, c store.RunCredential, replace bool) (bool, error) {
	if !validUUID(c.RunID) {
		return false, nil
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO work_run_credentials (run_id, user_login, sealed, expires_at)
		SELECT id, user_login, $3, $4 FROM work_runs
		WHERE id = $1 AND user_login = $2 AND status IN ('queued', 'running')
		ON CONFLICT (run_id) DO UPDATE SET sealed = EXCLUDED.sealed, expires_at = EXCLUDED.expires_at, updated_at = now()
		WHERE work_run_credentials.user_login = EXCLUDED.user_login
		  AND ($5 OR work_run_credentials.expires_at < EXCLUDED.expires_at)`,
		c.RunID, c.UserLogin, c.Sealed, c.ExpiresAt, replace)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// GetRunCredential implements CredentialStore.
func (s *PG) GetRunCredential(ctx context.Context, runID string) (*store.RunCredential, error) {
	if !validUUID(runID) {
		return nil, store.ErrNotFound
	}
	c := &store.RunCredential{}
	err := s.pool.QueryRow(ctx, `SELECT run_id::text, user_login, sealed, expires_at FROM work_run_credentials WHERE run_id = $1`, runID).
		Scan(&c.RunID, &c.UserLogin, &c.Sealed, &c.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// DeleteRunCredential implements CredentialStore.
func (s *PG) DeleteRunCredential(ctx context.Context, runID string) error {
	if !validUUID(runID) {
		return nil
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM work_run_credentials WHERE run_id = $1`, runID)
	return err
}

// PurgeRunCredentials implements CredentialStore.
func (s *PG) PurgeRunCredentials(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM work_run_credentials c
		WHERE c.expires_at <= now()
		   OR NOT EXISTS (SELECT 1 FROM work_runs r WHERE r.id = c.run_id AND r.status IN ('queued', 'running'))`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
