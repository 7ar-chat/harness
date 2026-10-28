package pgstore

import (
	"context"
	"encoding/json"
	"github.com/GhaziBenDahmane/harness/store"

	"github.com/google/uuid"
)

const ideaUsageCols = `id::text, user_login, coalesce(thread_id::text, ''), idea_id, action, surface, metadata, created_at`

// RecordIdeaUsage inserts a work_idea_usage row; the thread is kept only when it is the user's.
func (s *PG) RecordIdeaUsage(ctx context.Context, u store.IdeaUsage) (*store.IdeaUsage, error) {
	metadata := []byte("{}")
	if len(u.Metadata) > 0 {
		b, err := json.Marshal(store.CleanValue(u.Metadata))
		if err != nil {
			return nil, err
		}
		metadata = b
	}
	var threadID *string
	if _, err := uuid.Parse(u.ThreadID); err == nil {
		threadID = &u.ThreadID
	}
	row := s.pool.QueryRow(ctx, `INSERT INTO work_idea_usage (id, user_login, thread_id, idea_id, action, surface, metadata)
		VALUES ($1, $2, (SELECT id FROM work_threads WHERE id = $3::uuid AND user_login = $2), $4, $5, $6, $7)
		RETURNING `+ideaUsageCols,
		uuid.NewString(), u.UserLogin, threadID, store.CleanText(u.IdeaID), u.Action, store.CleanText(u.Surface), metadata)
	return scanIdeaUsage(row)
}

func scanIdeaUsage(row interface{ Scan(dest ...any) error }) (*store.IdeaUsage, error) {
	var u store.IdeaUsage
	var metadata []byte
	if err := row.Scan(&u.ID, &u.UserLogin, &u.ThreadID, &u.IdeaID, &u.Action, &u.Surface, &metadata, &u.CreatedAt); err != nil {
		return nil, err
	}
	if len(metadata) > 0 {
		_ = json.Unmarshal(metadata, &u.Metadata)
	}
	return &u, nil
}

// ListIdeaUsage returns a user's usage, newest first.
func (s *PG) ListIdeaUsage(ctx context.Context, login string) ([]*store.IdeaUsage, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+ideaUsageCols+` FROM work_idea_usage WHERE user_login = $1 ORDER BY created_at DESC, id`, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.IdeaUsage
	for rows.Next() {
		u, err := scanIdeaUsage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
