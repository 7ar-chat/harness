package pgstore

import (
	"context"
	"github.com/GhaziBenDahmane/harness/core/store"
	"time"
)

// threadNotifyCols / threadNotifyFrom add last_seen_at, the latest run and the
// unread flag to the thread query (see scanThread).
const threadNotifyCols = `t.last_seen_at, lr.id::text, lr.status, lr.finished_at, ` + threadUnreadCond

const threadNotifyFrom = `
	LEFT JOIN LATERAL (SELECT r.id, r.status, r.finished_at FROM work_runs r WHERE r.thread_id = t.id
		ORDER BY r.created_at DESC, r.id DESC LIMIT 1) lr ON true`

// threadUnreadCond: a run of thread t reached a terminal state after its owner last saw it.
const threadUnreadCond = `EXISTS (SELECT 1 FROM work_runs fr WHERE fr.thread_id = t.id AND fr.finished_at IS NOT NULL
		AND fr.finished_at > coalesce(t.last_seen_at, t.created_at))`

type threadNotify struct {
	lastSeen         *time.Time
	runID, runStatus *string
	runFinished      *time.Time
	unread           bool
}

func (n *threadNotify) dest() []any {
	return []any{&n.lastSeen, &n.runID, &n.runStatus, &n.runFinished, &n.unread}
}

func (n *threadNotify) apply(t *store.Thread) {
	t.LastSeenAt, t.Unread = n.lastSeen, n.unread
	if n.runID != nil {
		t.LastRun = &store.LastRun{ID: *n.runID, Status: *n.runStatus, FinishedAt: n.runFinished}
	}
}

func (s *PG) MarkThreadSeen(ctx context.Context, login, id string) error {
	if !validUUID(id) {
		return store.ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `UPDATE work_threads SET last_seen_at = now()
		WHERE id = $1 AND user_login = $2 AND deleted_at IS NULL`, id, login)
	if err == nil && tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return err
}

func (s *PG) CountUnreadThreads(ctx context.Context, login string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM work_threads t
		WHERE t.user_login = $1 AND t.deleted_at IS NULL AND `+threadUnreadCond, login).Scan(&n)
	return n, err
}

func (s *PG) SearchUnreadThreads(ctx context.Context, login, search string, limit int, before *store.ThreadCursor) ([]*store.Thread, error) {
	return s.searchThreads(ctx, login, search, true, limit, before)
}
