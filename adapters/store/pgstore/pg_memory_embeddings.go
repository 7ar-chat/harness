package pgstore

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/GhaziBenDahmane/harness/core/store"
)

var _ store.MemoryEmbeddingStore = (*PG)(nil)

func (s *PG) PutMemoryEmbeddings(ctx context.Context, login string, items []store.EntryEmbedding) error {
	batch := &pgx.Batch{}
	for _, it := range items {
		batch.Queue(`INSERT INTO work_memory_embeddings (user_login, entry_id, model, content_hash, vec) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (user_login, entry_id) DO UPDATE SET model = $3, content_hash = $4, vec = $5, updated_at = now()`,
			login, it.EntryID, it.Model, it.ContentHash, it.Vec)
	}
	res := s.pool.SendBatch(ctx, batch)
	defer res.Close()
	for range items {
		if _, err := res.Exec(); err != nil {
			return err
		}
	}
	return nil
}

func (s *PG) GetMemoryEmbeddings(ctx context.Context, login string) (map[string]store.EntryEmbedding, error) {
	rows, err := s.pool.Query(ctx, `SELECT v.entry_id, v.model, v.content_hash, v.vec FROM work_memory_embeddings v
		JOIN work_memory_entries e ON e.user_login = v.user_login AND e.id = v.entry_id
		WHERE v.user_login = $1 AND e.status IN ('active', 'pending')`, login)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]store.EntryEmbedding{}
	for rows.Next() {
		var it store.EntryEmbedding
		if err := rows.Scan(&it.EntryID, &it.Model, &it.ContentHash, &it.Vec); err != nil {
			return nil, err
		}
		out[it.EntryID] = it
	}
	return out, rows.Err()
}
