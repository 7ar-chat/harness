package pgstore

import (
	"context"

	"github.com/GhaziBenDahmane/harness/core/sandbox"
)

// SwapThreadSandbox sets sandbox_id to sandboxID when it currently equals
// expected and the thread is not deleted, reporting whether it did (the
// compare-and-set that decides which sandbox a thread keeps; stream A, C-10).
func (s *PG) SwapThreadSandbox(ctx context.Context, threadID, expected, sandboxID string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE work_threads SET sandbox_id = nullif($3, '')
		WHERE id = $1 AND coalesce(sandbox_id, '') = $2 AND deleted_at IS NULL`, threadID, expected, sandboxID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// UserSandboxes lists the user's live threads with a recorded sandbox (what
// CountUserSandboxes counts), for the quota's reconcile of dead sandboxes (C-09).
func (s *PG) UserSandboxes(ctx context.Context, login string) ([]sandbox.UserSandbox, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text, sandbox_id FROM work_threads
		WHERE user_login = $1 AND sandbox_id IS NOT NULL AND deleted_at IS NULL ORDER BY updated_at, id`, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sandbox.UserSandbox
	for rows.Next() {
		var u sandbox.UserSandbox
		if err := rows.Scan(&u.ThreadID, &u.SandboxID); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
