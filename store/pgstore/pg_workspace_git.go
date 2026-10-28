package pgstore

import (
	"context"
	"errors"
	"github.com/GhaziBenDahmane/harness/store"

	"github.com/jackc/pgx/v5"
)

var _ store.WorkspaceGitStore = (*PG)(nil)

// WorkspaceGit implements WorkspaceGitStore.
func (s *PG) WorkspaceGit(ctx context.Context, threadID string) (*store.WorkspaceGitArchive, error) {
	a := &store.WorkspaceGitArchive{ThreadID: threadID}
	err := s.pool.QueryRow(ctx, `SELECT blob_key, head, size_bytes, updated_at FROM work_workspace_git WHERE thread_id = $1`,
		threadID).Scan(&a.BlobKey, &a.Head, &a.SizeBytes, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

// PutWorkspaceGit implements WorkspaceGitStore (the 0007 trigger queues the
// replaced blob for garbage collection). A thread that no longer exists is ErrNotFound.
func (s *PG) PutWorkspaceGit(ctx context.Context, a store.WorkspaceGitArchive) error {
	tag, err := s.pool.Exec(ctx, `INSERT INTO work_workspace_git (thread_id, blob_key, head, size_bytes, updated_at)
		SELECT id, $2, $3, $4, now() FROM work_threads WHERE id = $1
		ON CONFLICT (thread_id) DO UPDATE SET blob_key = EXCLUDED.blob_key, head = EXCLUDED.head,
			size_bytes = EXCLUDED.size_bytes, updated_at = now()`, a.ThreadID, a.BlobKey, a.Head, a.SizeBytes)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	// A key reused by the new archive must not stay queued for collection.
	_, err = s.pool.Exec(ctx, `DELETE FROM work_blob_gc WHERE key = $1`, a.BlobKey)
	return err
}

// WorkspaceGitIssue implements WorkspaceGitStore.
func (s *PG) WorkspaceGitIssue(ctx context.Context, threadID string) (*store.WorkspaceGitIssue, error) {
	i := &store.WorkspaceGitIssue{ThreadID: threadID}
	err := s.pool.QueryRow(ctx, `SELECT reason, head, size_bytes, updated_at FROM work_workspace_git_issues WHERE thread_id = $1`,
		threadID).Scan(&i.Reason, &i.Head, &i.SizeBytes, &i.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return i, nil
}

// SetWorkspaceGitIssue implements WorkspaceGitStore.
func (s *PG) SetWorkspaceGitIssue(ctx context.Context, i store.WorkspaceGitIssue) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO work_workspace_git_issues (thread_id, reason, head, size_bytes, updated_at)
		SELECT id, $2, $3, $4, now() FROM work_threads WHERE id = $1
		ON CONFLICT (thread_id) DO UPDATE SET reason = EXCLUDED.reason, head = EXCLUDED.head,
			size_bytes = EXCLUDED.size_bytes, updated_at = now()`, i.ThreadID, store.CleanText(i.Reason), store.CleanText(i.Head), i.SizeBytes)
	return err
}

// ClearWorkspaceGitIssue implements WorkspaceGitStore.
func (s *PG) ClearWorkspaceGitIssue(ctx context.Context, threadID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM work_workspace_git_issues WHERE thread_id = $1`, threadID)
	return err
}
