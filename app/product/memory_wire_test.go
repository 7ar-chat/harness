package product

import (
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

// The memory pages' answers used to be built from maps; these are the bytes those maps encoded to (keys sorted),
// which the named structs behind the generated TypeScript types must keep.

func TestEntryViewEncodesLikeTheMapItReplaced(t *testing.T) {
	// Arrange
	imp := 0.5
	at := time.Date(2026, 10, 6, 11, 30, 0, 123000000, time.UTC)
	full := &store.MemoryEntry{ID: "mem_1", Path: "memories/semantic/facts/a.md", Title: "T <b>", Content: "c", Tags: []string{"a"}, Importance: &imp,
		Metadata: map[string]any{"pinned": true, "history": []any{map[string]any{"title": "o", "content": "x"}}}, Scope: "global", Repository: "r", Source: "user",
		Status: store.MemoryPending, SourceThreadID: "th1", CreatedAt: at, UpdatedAt: at, ContentUpdatedAt: at,
		Proposal: &store.MemoryProposal{Path: "p", Title: "t", Content: "c", Tags: []string{"z"}, ProposedAt: at}}

	// Act
	gotFull, _ := json.Marshal(entryView(full))
	gotEmpty, _ := json.Marshal(entryView(&store.MemoryEntry{ID: "mem_2"}))

	// Assert
	wantFull := `{"content":"c","created_at":"2026-10-06T11:30:00.123Z","embedding_updated_at":"2026-10-06T11:30:00.123Z","id":"mem_1","importance":0.5,` +
		`"metadata":{"history":[{"content":"x","title":"o"}],"pinned":true},"path":"memories/semantic/facts/a.md",` +
		`"proposal":{"path":"p","title":"t","content":"c","tags":["z"],"importance":null,"metadata":null,"content_hash":"","proposed_at":"2026-10-06T11:30:00.123Z"},` +
		`"repository":"r","scope":"global","source":"user","sourceThreadId":"th1","status":"pending","tags":["a"],"title":"T \u003cb\u003e","updated_at":"2026-10-06T11:30:00.123Z"}`
	wantEmpty := `{"content":"","created_at":null,"embedding_updated_at":null,"id":"mem_2","importance":null,"metadata":{},"path":"","repository":"","scope":"","source":"","status":"","tags":[],"title":"","updated_at":null}`
	if string(gotFull) != wantFull {
		t.Fatalf("full\n got %s\nwant %s", gotFull, wantFull)
	}
	if string(gotEmpty) != wantEmpty {
		t.Fatalf("empty\n got %s\nwant %s", gotEmpty, wantEmpty)
	}
}

func TestMemoryListAndStatsKeepTheirWireShape(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{})
	body := func(path string) string {
		r.t.Helper()
		resp := r.do("GET", path, "alice", "")
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			r.t.Fatalf("GET %s = %d: %s", path, resp.StatusCode, b)
		}
		return string(b)
	}
	zeroService := `{"recallRuns":0,"recallItems":0,"recallChars":0,"recallPinned":0,"searches":0,"searchHits":0,"extractRuns":0,"extractProposed":0,"extractSkipped":0,"extractErrors":0,"embedded":0,"embedErrors":0}`

	// Act
	emptyList := body("/api/memory")
	emptyStats := body("/api/memory/stats")
	r.json("POST", "/api/memory", "alice", `{"title":"Style","content":"Terse answers.","pinned":true,"path":"memories/semantic/preferences/style.md"}`, 201)
	stats := body("/api/memory/stats")
	list := body("/api/memory")

	// Assert
	if emptyList != `{"enabled":true,"entries":[]}`+"\n" {
		t.Fatalf("empty list = %q", emptyList)
	}
	if want := `{"active":0,"byKind":{},"embeddings":{"enabled":false},"pending":0,"pinned":0,"service":` + zeroService + "}\n"; emptyStats != want {
		t.Fatalf("empty stats\n got %q\nwant %q", emptyStats, want)
	}
	if want := `{"active":1,"byKind":{"preferences":1},"embeddings":{"enabled":false},"pending":0,"pinned":1,"service":` + zeroService + "}\n"; stats != want {
		t.Fatalf("stats\n got %q\nwant %q", stats, want)
	}
	var got struct {
		Enabled bool
		Entries []map[string]any
	}
	if err := json.Unmarshal([]byte(list), &got); err != nil || !got.Enabled || len(got.Entries) != 1 {
		t.Fatalf("list = %s (%v)", list, err)
	}
	e := got.Entries[0]
	for _, k := range []string{"content", "created_at", "embedding_updated_at", "id", "importance", "metadata", "path", "repository", "scope", "source", "status", "tags", "title", "updated_at"} {
		if _, ok := e[k]; !ok {
			t.Errorf("entry lacks %q: %v", k, e)
		}
	}
	if _, ok := e["proposal"]; ok {
		t.Errorf("an entry without a proposal must not carry the key: %v", e)
	}
}
