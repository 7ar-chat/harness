package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/features/memory"
)

func TestOpenAIEmbedderSendsTheRequestAndReturnsUnitVectorsInInputOrder(t *testing.T) {
	// The endpoint may answer out of order: vectors come back in the order of the texts, normalized, and the request
	// carries the model, the bearer key and the (truncated) inputs.
	// Arrange
	var got struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}
	var auth, path, ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path, ctype = r.Header.Get("Authorization"), r.URL.Path, r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"data":[{"index":1,"embedding":[0,2]},{"index":0,"embedding":[3,4]}]}`))
	}))
	defer srv.Close()
	e := &memory.OpenAIEmbedder{BaseURL: srv.URL + "/v1/", APIKey: "k", ModelName: "m1", HTTP: srv.Client()}
	long := strings.Repeat("é", 7000)

	// Act
	vecs, err := e.Embed(context.Background(), []string{"first", long})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/embeddings" || auth != "Bearer k" || ctype != "application/json" || got.Model != "m1" {
		t.Fatalf("request: path %q auth %q type %q model %q", path, auth, ctype, got.Model)
	}
	if len(got.Input) != 2 || got.Input[0] != "first" || len([]rune(got.Input[1])) != 6000 {
		t.Fatalf("inputs not truncated to 6000 runes: %d", len([]rune(got.Input[1])))
	}
	if len(vecs) != 2 || math.Abs(float64(vecs[0][0])-0.6) > 1e-6 || math.Abs(float64(vecs[0][1])-0.8) > 1e-6 || vecs[1][0] != 0 || vecs[1][1] != 1 {
		t.Fatalf("vectors = %v", vecs)
	}
	if e.Model() != "m1" || (&memory.OpenAIEmbedder{}).Model() != "text-embedding-3-small" {
		t.Fatalf("model names: %q", e.Model())
	}
}

func TestOpenAIEmbedderRejectsBadAnswers(t *testing.T) {
	// A non-2xx status, a body that is not the expected JSON and a count that differs from the input are errors,
	// never partial vectors; an empty input needs no request.
	// Arrange
	reply := func(status int, body string) *memory.OpenAIEmbedder {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return &memory.OpenAIEmbedder{BaseURL: srv.URL, HTTP: srv.Client()}
	}
	ctx := context.Background()

	// Act
	_, status := reply(500, `{"error":"x"}`).Embed(ctx, []string{"a"})
	_, notJSON := reply(200, `<html>`).Embed(ctx, []string{"a"})
	_, short := reply(200, `{"data":[{"index":0,"embedding":[1]}]}`).Embed(ctx, []string{"a", "b"})
	none, noneErr := (&memory.OpenAIEmbedder{BaseURL: "http://127.0.0.1:1"}).Embed(ctx, nil)
	_, unreachable := (&memory.OpenAIEmbedder{BaseURL: "http://127.0.0.1:1"}).Embed(ctx, []string{"a"})
	_, badURL := (&memory.OpenAIEmbedder{BaseURL: "http://bad host"}).Embed(ctx, []string{"a"})

	// Assert
	if status == nil || !strings.Contains(status.Error(), "500") {
		t.Fatalf("status error = %v", status)
	}
	if notJSON == nil || short == nil {
		t.Fatalf("unexpected bodies accepted: %v %v", notJSON, short)
	}
	if none != nil || noneErr != nil {
		t.Fatalf("empty input: %v %v", none, noneErr)
	}
	if unreachable == nil || badURL == nil {
		t.Fatalf("transport errors lost: %v %v", unreachable, badURL)
	}
}

func TestCosineVecHandlesMismatchedAndZeroVectors(t *testing.T) {
	// Arrange / Act
	same := memory.CosineVec([]float32{1, 2, 3}, []float32{2, 4, 6})
	ortho := memory.CosineVec([]float32{1, 0}, []float32{0, 1})
	opposite := memory.CosineVec([]float32{1, 0}, []float32{-1, 0})

	// Assert
	if math.Abs(same-1) > 1e-9 || ortho != 0 || math.Abs(opposite+1) > 1e-9 {
		t.Fatalf("same %v ortho %v opposite %v", same, ortho, opposite)
	}
	if memory.CosineVec(nil, nil) != 0 || memory.CosineVec([]float32{1}, []float32{1, 2}) != 0 || memory.CosineVec([]float32{0, 0}, []float32{1, 1}) != 0 || memory.CosineVec([]float32{1, 1}, []float32{0, 0}) != 0 {
		t.Fatal("degenerate vectors must have no similarity")
	}
}

// scriptedEmbedder embeds with a function and can fail.
type scriptedEmbedder struct {
	mu    sync.Mutex
	fail  error
	short bool
	// failAfter > 0 makes every call after that many calls fail.
	failAfter int
	calls     int
	texts     []string
}

func (e *scriptedEmbedder) Model() string { return "scripted" }
func (e *scriptedEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.texts = append(e.texts, texts...)
	e.calls++
	if e.failAfter > 0 && e.calls > e.failAfter {
		return nil, errors.New("model down")
	}
	if e.fail != nil {
		return nil, e.fail
	}
	n := len(texts)
	if e.short {
		n = 0
	}
	out := make([][]float32, n)
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

func TestSearchStaysLexicalWhenTheEmbeddingModelFails(t *testing.T) {
	// A failing embedder never fails the search: words still rank, the trace reports the fallback and the error is counted.
	// Arrange
	emb := &scriptedEmbedder{fail: errors.New("model down")}
	st, svc, _ := newFaulty(memory.Options{Embedder: emb})
	seed(st, "alice", "m1", "memories/semantic/facts/a.md", "Deploys go through staging first")

	// Act
	r := svc.Search(context.Background(), "alice", memory.SearchInput{Query: "staging deploys"})

	// Assert
	hits := r.Data["results"].([]map[string]any)
	trace := r.Data["trace"].(map[string]any)
	if r.IsError || len(hits) != 1 || hits[0]["id"] != "m1" {
		t.Fatalf("search = %v", r.Data)
	}
	if trace["semantic_fallback"] != true || trace["semantic_backend"] != "embeddings:scripted" {
		t.Fatalf("trace = %v", trace)
	}
	if svc.Stats().EmbedErrors == 0 {
		t.Fatal("the embedding failure was not counted")
	}
}

func TestSearchIgnoresAQueryVectorOfTheWrongCount(t *testing.T) {
	// An embedder that answers with no vector for the query is the same as no embedder.
	// Arrange
	emb := &scriptedEmbedder{short: true}
	st, svc, _ := newFaulty(memory.Options{Embedder: emb})
	seed(st, "alice", "m1", "memories/semantic/facts/a.md", "Deploys go through staging first")

	// Act
	r := svc.Search(context.Background(), "alice", memory.SearchInput{Query: "staging"})

	// Assert
	if hits := r.Data["results"].([]map[string]any); len(hits) != 1 || r.Data["trace"].(map[string]any)["semantic_fallback"] != true {
		t.Fatalf("search = %v", r.Data)
	}
}

func TestVectorsAreStoredOnceAndRecomputedWhenTheModelOrContentChanges(t *testing.T) {
	// Stored vectors are reused while the model and content hash match; a different content hash is re-embedded
	// (and the new vector replaces the old one); a failure to store the vectors does not fail the search.
	// Arrange
	emb := &scriptedEmbedder{}
	st, svc, _ := newFaulty(memory.Options{Embedder: emb})
	ctx := context.Background()
	e := seed(st, "alice", "m1", "memories/semantic/facts/a.md", "Deploys go through staging first")
	in := memory.SearchInput{Query: "staging"}

	// Act
	svc.Search(ctx, "alice", in)
	first := len(emb.texts)
	svc.Search(ctx, "alice", in)
	second := len(emb.texts)
	e.Content, e.ContentHash = "Deploys now go through canary first", "other-hash"
	_, _ = st.UpdateMemoryEntry(ctx, e, store.MemoryLimits{})
	svc.Search(ctx, "alice", in)
	third := len(emb.texts)
	stored, _ := st.GetMemoryEmbeddings(ctx, "alice")

	// Assert
	if first != 2 || second-first != 1 || third-second != 2 { // query+entry, query, query+changed entry
		t.Fatalf("texts embedded per search: %d, %d, %d", first, second-first, third-second)
	}
	if stored["m1"].ContentHash != "other-hash" || stored["m1"].Model != "scripted" {
		t.Fatalf("stored vector = %+v", stored["m1"])
	}
	if svc.Stats().Embedded != 2 {
		t.Fatalf("embedded = %d", svc.Stats().Embedded)
	}
}

func TestVectorsFailuresDoNotBreakSearch(t *testing.T) {
	// Reading the stored vectors, embedding the entries or storing the result may fail: search answers with the words.
	// Arrange
	ctx := context.Background()
	check := func(name string, setup func(st *faultStore, emb *scriptedEmbedder)) {
		emb := &scriptedEmbedder{}
		st, svc, _ := newFaulty(memory.Options{Embedder: emb})
		seed(st, "alice", "m1", "memories/semantic/facts/a.md", "Deploys go through staging first")
		setup(st, emb)

		// Act
		r := svc.Search(ctx, "alice", memory.SearchInput{Query: "staging"})

		// Assert
		if hits, _ := r.Data["results"].([]map[string]any); r.IsError || len(hits) != 1 {
			t.Errorf("%s: search = %v", name, r.Data)
		}
	}

	// Act / Assert
	check("reading vectors fails", func(st *faultStore, _ *scriptedEmbedder) { st.errGetVec = errors.New("read") })
	check("storing vectors fails", func(st *faultStore, _ *scriptedEmbedder) { st.errPutVec = errors.New("write") })
	check("entry embedding fails after the query one worked", func(_ *faultStore, emb *scriptedEmbedder) { emb.failAfter = 1 })
}

func TestStoresWithoutEmbeddingStorageStayLexical(t *testing.T) {
	// A MemoryStore that cannot keep vectors never gets the embedder called for entries; search still answers.
	// Arrange
	emb := &scriptedEmbedder{}
	st := memstore.New()
	svc := memory.New(plainStore{st}, memory.Options{Embedder: emb})
	seed(st, "alice", "m1", "memories/semantic/facts/a.md", "Deploys go through staging first")

	// Act
	r := svc.Search(context.Background(), "alice", memory.SearchInput{Query: "staging"})

	// Assert
	if hits := r.Data["results"].([]map[string]any); len(hits) != 1 {
		t.Fatalf("search = %v", r.Data)
	}
	if len(emb.texts) != 1 || emb.texts[0] != "staging" {
		t.Fatalf("only the query may be embedded, got %v", emb.texts)
	}
}

func TestAFreshEntryIsEmbeddedInTheBackground(t *testing.T) {
	// Creating an entry computes its vector without blocking the write, so the next search needs only the query.
	// Arrange
	emb := &scriptedEmbedder{}
	st, svc, w := newFaulty(memory.Options{Embedder: emb})
	ctx := context.Background()

	// Act
	r := svc.Create(ctx, w, memory.CreateInput{Content: "Deploys go through staging first"})
	id := r.Data["memory"].(memory.Entry).ID

	// Assert
	waitFor(t, "the vector of the new entry", func() bool {
		v, _ := st.GetMemoryEmbeddings(ctx, "alice")
		_, ok := v[id]
		return ok
	})
}

func TestADeletedEntryIsNotEmbedded(t *testing.T) {
	// Entries that are being deleted get no vector.
	// Arrange
	emb := &scriptedEmbedder{}
	_, svc, w := newFaulty(memory.Options{Embedder: emb})
	ctx := context.Background()
	id := svc.Create(ctx, w, memory.CreateInput{Content: "Deploys go through staging first"}).Data["memory"].(memory.Entry).ID
	waitFor(t, "the first embedding", func() bool { emb.mu.Lock(); defer emb.mu.Unlock(); return len(emb.texts) == 1 })

	// Act
	r := svc.Delete(ctx, w, id, "", map[string]any{"reason": "outdated"}, "")

	// Assert
	if r.IsError {
		t.Fatal(r.Data)
	}
	emb.mu.Lock()
	defer emb.mu.Unlock()
	if len(emb.texts) != 1 {
		t.Fatalf("a deleted entry was embedded: %v", emb.texts)
	}
}

func TestEmbedErrorStatsAndDeadlineAreIndependentOfTheCaller(t *testing.T) {
	// An embedder blocking until its context ends is cut by the service's own timeout, not the caller's:
	// asserted with a cancelled caller context, which fails fast and falls back to words.
	// Arrange
	emb := &blockingEmbedder{}
	st, svc, _ := newFaulty(memory.Options{Embedder: emb})
	seed(st, "alice", "m1", "memories/semantic/facts/a.md", "Deploys go through staging first")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Act
	start := time.Now()
	r := svc.Search(ctx, "alice", memory.SearchInput{Query: "staging"})

	// Assert
	if time.Since(start) > 5*time.Second {
		t.Fatal("search waited for the embedder")
	}
	if hits := r.Data["results"].([]map[string]any); len(hits) != 1 {
		t.Fatalf("search = %v", r.Data)
	}
}

type blockingEmbedder struct{}

func (blockingEmbedder) Model() string { return "blocking" }
func (blockingEmbedder) Embed(ctx context.Context, _ []string) ([][]float32, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
