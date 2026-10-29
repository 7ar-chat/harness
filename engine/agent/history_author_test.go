package agent

import (
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/core/store"
)

func userMsg(pos int64, text, author string) *store.Message {
	return &store.Message{ID: text, Role: store.RoleUser, Content: text, Position: pos, AuthorLogin: author}
}

func TestHistoryTagsAuthorsOnlyInSharedThreads(t *testing.T) {
	single := BuildHistory([]*store.Message{userMsg(1, "hello", "alice"), userMsg(2, "again", "alice")})
	for _, m := range single {
		if strings.HasPrefix(m.Content, "[") {
			t.Fatalf("single-author thread tagged: %q", m.Content)
		}
	}
	shared := BuildHistory([]*store.Message{userMsg(1, "hello", "alice"), userMsg(2, "hi", "bob"),
		{ID: "s", Role: store.RoleUser, Content: "stop", Position: 3, AuthorKind: store.AuthorSupervisor, OnBehalfOf: "bob",
			Metadata: map[string]any{"steer": true}}})
	var got []string
	for _, m := range shared {
		got = append(got, m.Content)
	}
	joined := strings.Join(got, "|")
	for _, want := range []string{"[alice] hello", "[bob] hi", "[supervisor, for bob] stop"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("history %q lacks %q", joined, want)
		}
	}
	// A thread from before authorship (no author) is untouched.
	old := BuildHistory([]*store.Message{userMsg(1, "legacy", "")})
	if old[0].Content != "legacy" {
		t.Fatalf("legacy = %q", old[0].Content)
	}
}
