package memory_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/features/memory"
)

// The "memory" object of the tool results and the memory pages used to be a map; these are the bytes that map
// encoded to (keys sorted, null for a zero time or a missing importance), which the Entry struct must keep.
const (
	fullEntryJSON  = `{"created_at":"2026-10-06T11:30:00.123Z","embedding_updated_at":null,"id":"mem_1","importance":0.5,"metadata":{"history":[{"content":"x","title":"o"}],"pinned":true},"path":"memories/semantic/facts/a.md","repository":"r","scope":"global","source":"user","status":"active","tags":["a"],"title":"T \u003cb\u003e","updated_at":"2026-10-06T11:30:00.123Z"}`
	emptyEntryJSON = `{"created_at":null,"embedding_updated_at":null,"id":"mem_2","importance":null,"metadata":{},"path":"","repository":"","scope":"","source":"","status":"","tags":[],"title":"","updated_at":null}`
)

func TestPublicEntryEncodesLikeTheMapItReplaced(t *testing.T) {
	// Arrange
	imp := 0.5
	at := time.Date(2026, 10, 6, 12, 30, 0, 123000000, time.FixedZone("x", 3600))
	full := &store.MemoryEntry{ID: "mem_1", Path: "memories/semantic/facts/a.md", Title: "T <b>", Content: "not public", Tags: []string{"a"}, Importance: &imp,
		Metadata: map[string]any{"pinned": true, "history": []any{map[string]any{"title": "o", "content": "x"}}}, Scope: "global", Repository: "r",
		Source: "user", Status: store.MemoryActive, SourceThreadID: "th1", CreatedAt: at, UpdatedAt: at}
	empty := &store.MemoryEntry{ID: "mem_2"}

	// Act
	gotFull, errFull := json.Marshal(memory.PublicEntry(full))
	gotEmpty, errEmpty := json.Marshal(memory.PublicEntry(empty))

	// Assert
	if errFull != nil || errEmpty != nil {
		t.Fatal(errFull, errEmpty)
	}
	if string(gotFull) != fullEntryJSON {
		t.Fatalf("full entry\n got %s\nwant %s", gotFull, fullEntryJSON)
	}
	if string(gotEmpty) != emptyEntryJSON {
		t.Fatalf("empty entry\n got %s\nwant %s", gotEmpty, emptyEntryJSON)
	}
}

func TestPublicEntryDoesNotShareStateWithTheStoredEntry(t *testing.T) {
	// Arrange
	imp := 0.25
	e := &store.MemoryEntry{ID: "mem_3", Importance: &imp, Metadata: map[string]any{"pinned": true}}

	// Act
	pub := memory.PublicEntry(e)
	imp = 0.9
	pub.Metadata["pinned"] = false

	// Assert
	if *pub.Importance != 0.25 || e.Metadata["pinned"] != true {
		t.Fatalf("public entry aliases the stored one: importance %v, stored metadata %v", *pub.Importance, e.Metadata)
	}
}
