package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

// Embedder turns texts into vectors. With one, search and recall compare the
// meaning of a query with the entries, not only their words.
type Embedder interface {
	// Model names the embedding model; vectors of another model are recomputed.
	Model() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// OpenAIEmbedder calls an OpenAI-compatible /embeddings endpoint.
type OpenAIEmbedder struct {
	BaseURL   string // default https://api.openai.com/v1
	APIKey    string
	ModelName string // default text-embedding-3-small
	HTTP      *http.Client
}

func (e *OpenAIEmbedder) Model() string {
	if e.ModelName != "" {
		return e.ModelName
	}
	return "text-embedding-3-small"
}

// Embed returns unit-length vectors (so a dot product is the cosine).
func (e *OpenAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	base := strings.TrimRight(e.BaseURL, "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	in := make([]string, len(texts))
	for i, t := range texts {
		in[i] = truncateRunes(t, 6000)
	}
	body, _ := json.Marshal(map[string]any{"model": e.Model(), "input": in})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	hc := e.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embeddings: status %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Data) != len(texts) {
		return nil, errors.New("embeddings: unexpected response")
	}
	sort.Slice(out.Data, func(i, j int) bool { return out.Data[i].Index < out.Data[j].Index })
	vecs := make([][]float32, len(texts))
	for i, d := range out.Data {
		vecs[i] = unit(d.Embedding)
	}
	return vecs, nil
}

func unit(v []float32) []float32 {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	if n == 0 {
		return v
	}
	n = math.Sqrt(n)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / n)
	}
	return out
}

// CosineVec is the cosine of two vectors (0 when their sizes differ).
func CosineVec(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// vecSimilarity maps a cosine of embeddings to 0..1: unrelated texts sit
// around 0.1 to 0.2 for the usual models, related ones above 0.4.
func vecSimilarity(c float64) float64 {
	s := (c - 0.15) / 0.55
	return math.Max(0, math.Min(1, s))
}

func embedText(e *store.MemoryEntry) string {
	return truncateRunes(e.Title+"\n"+e.Content, 6000)
}

// vectors returns the embeddings of entries (of the memory at key), computing
// and storing the missing or stale ones (at most 64 per call). Without an
// embedder or a store that keeps them it returns nil and search stays lexical.
func (s *Service) vectors(ctx context.Context, key string, entries []*store.MemoryEntry) map[string][]float32 {
	emb := s.o.Embedder
	vs, ok := s.st.(store.MemoryEmbeddingStore)
	if emb == nil || !ok || len(entries) == 0 {
		return nil
	}
	have, err := vs.GetMemoryEmbeddings(ctx, key)
	if err != nil {
		slog.Warn("memory: reading embeddings failed", "error", err)
		return nil
	}
	out := make(map[string][]float32, len(entries))
	var stale []*store.MemoryEntry
	for _, e := range entries {
		if v, ok := have[e.ID]; ok && v.Model == emb.Model() && v.ContentHash == e.ContentHash {
			out[e.ID] = v.Vec
			continue
		}
		stale = append(stale, e)
	}
	if len(stale) > 64 {
		stale = stale[:64]
	}
	if len(stale) == 0 {
		return out
	}
	texts := make([]string, len(stale))
	for i, e := range stale {
		texts[i] = embedText(e)
	}
	ectx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	vecs, err := emb.Embed(ectx, texts)
	if err != nil {
		slog.Warn("memory: embedding failed; search stays lexical for these entries", "error", err)
		s.stats.embedErrors.Add(1)
		return out
	}
	items := make([]store.EntryEmbedding, len(stale))
	for i, e := range stale {
		items[i] = store.EntryEmbedding{EntryID: e.ID, Model: emb.Model(), ContentHash: e.ContentHash, Vec: vecs[i]}
		out[e.ID] = vecs[i]
	}
	if err := vs.PutMemoryEmbeddings(ctx, key, items); err != nil {
		slog.Warn("memory: storing embeddings failed", "error", err)
	}
	s.stats.embedded.Add(int64(len(items)))
	return out
}

// embedQuery embeds a search query (nil without an embedder, or on failure).
func (s *Service) embedQuery(ctx context.Context, q string) []float32 {
	if s.o.Embedder == nil || strings.TrimSpace(q) == "" {
		return nil
	}
	ectx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	vecs, err := s.o.Embedder.Embed(ectx, []string{q})
	if err != nil || len(vecs) != 1 {
		if err != nil {
			slog.Warn("memory: embedding the query failed", "error", err)
			s.stats.embedErrors.Add(1)
		}
		return nil
	}
	return vecs[0]
}

// embedAsync embeds a fresh entry in the background so the next search has its vector.
func (s *Service) embedAsync(key string, e *store.MemoryEntry) {
	if s.o.Embedder == nil || e == nil || e.Status == store.MemoryDeleted {
		return
	}
	cp := e.Clone()
	s.embedding.Add(1)
	go func() {
		defer s.embedding.Done()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		s.vectors(ctx, key, []*store.MemoryEntry{cp})
	}()
}

// WaitEmbeddings blocks until the background embeddings started by writes so far have finished. A host may call it before
// shutting down; tests use it to observe a settled state.
func (s *Service) WaitEmbeddings() { s.embedding.Wait() }
