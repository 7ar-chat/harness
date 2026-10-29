package store

import "context"

// EntryEmbedding is the vector of a memory entry: computed from the entry's
// title and content by Model, valid while ContentHash is the entry's.
type EntryEmbedding struct {
	EntryID     string
	Model       string
	ContentHash string
	Vec         []float32
}

// MemoryEmbeddingStore is implemented by stores that keep memory embeddings
// (the memory service type-asserts it; without it search stays lexical).
type MemoryEmbeddingStore interface {
	// PutMemoryEmbeddings stores or replaces the vectors of the user's entries.
	PutMemoryEmbeddings(ctx context.Context, login string, items []EntryEmbedding) error
	// GetMemoryEmbeddings returns the user's vectors by entry id.
	GetMemoryEmbeddings(ctx context.Context, login string) (map[string]EntryEmbedding, error)
}
