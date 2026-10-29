package memstore

import (
	"context"

	"github.com/GhaziBenDahmane/harness/store"
)

var _ store.MemoryEmbeddingStore = (*Store)(nil)

func (s *Store) PutMemoryEmbeddings(_ context.Context, login string, items []store.EntryEmbedding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mem.vectors == nil {
		s.mem.vectors = map[string]map[string]store.EntryEmbedding{}
	}
	if s.mem.vectors[login] == nil {
		s.mem.vectors[login] = map[string]store.EntryEmbedding{}
	}
	for _, it := range items {
		it.Vec = append([]float32(nil), it.Vec...)
		s.mem.vectors[login][it.EntryID] = it
	}
	return nil
}

func (s *Store) GetMemoryEmbeddings(_ context.Context, login string) (map[string]store.EntryEmbedding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]store.EntryEmbedding, len(s.mem.vectors[login]))
	for id, it := range s.mem.vectors[login] {
		out[id] = it
	}
	return out, nil
}
