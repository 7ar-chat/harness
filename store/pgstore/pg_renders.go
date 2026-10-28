package pgstore

import (
	"context"
	"errors"
	"github.com/GhaziBenDahmane/harness/store"
	"slices"

	"github.com/jackc/pgx/v5"
)

func (s *PG) PutRender(ctx context.Context, r *store.Render) error {
	if !validUUID(r.ThreadID) {
		return store.ErrNotFound
	}
	pages := slices.Clone(r.Pages)
	slices.Sort(pages)
	pages = slices.Compact(pages)
	_, err := s.pool.Exec(ctx, `INSERT INTO work_renders (thread_id, render_id, source_path, label, allowed, pages)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (thread_id, render_id) DO UPDATE SET source_path = EXCLUDED.source_path, label = EXCLUDED.label,
			allowed = EXCLUDED.allowed, updated_at = now(),
			pages = (SELECT coalesce(array_agg(DISTINCT p ORDER BY p), '{}') FROM unnest(work_renders.pages || EXCLUDED.pages) p)`,
		r.ThreadID, r.RenderID, store.CleanText(r.SourcePath), store.CleanText(r.Label), r.Allowed, pages)
	return err
}

func (s *PG) GetRender(ctx context.Context, threadID, renderID string) (*store.Render, error) {
	if !validUUID(threadID) {
		return nil, store.ErrNotFound
	}
	r := store.Render{ThreadID: threadID, RenderID: renderID}
	var pages []int32
	err := s.pool.QueryRow(ctx, `SELECT source_path, label, allowed, pages, created_at, updated_at FROM work_renders
		WHERE thread_id = $1 AND render_id = $2`, threadID, renderID).Scan(&r.SourcePath, &r.Label, &r.Allowed, &pages, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	for _, p := range pages {
		r.Pages = append(r.Pages, int(p))
	}
	return &r, nil
}
