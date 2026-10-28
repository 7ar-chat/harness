package memory_test

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/memory"
	"github.com/GhaziBenDahmane/harness/store"
)

var now = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func entry(id, path, content string, updated time.Time, tags ...string) *store.MemoryEntry {
	return &store.MemoryEntry{ID: id, UserLogin: "alice", Path: path, Content: content, Tags: tags, Scope: "personal",
		Repository: "default", Source: store.MemorySourceProxy, Status: store.MemoryActive, CreatedAt: updated, UpdatedAt: updated}
}

func TestTokenizeSplitChunksAndSnippetFollowWork(t *testing.T) {
	// Tokens are lowercased ASCII words with _ . / : -; chunks are paragraphs and list items without headings; snippets are 240 characters.
	// Act
	tokens := memory.Tokenize("Deploy via `make release` to PROD-eu/v2: done!")
	chunks := memory.SplitChunks("# Facts\n- First fact\n* Second\n\nA paragraph\non two lines\n\n## End")
	long := memory.Snippet(strings.Repeat("word ", 100))

	// Assert
	if !reflect.DeepEqual(tokens, []string{"deploy", "via", "make", "release", "to", "prod-eu/v2:", "done"}) {
		t.Fatalf("tokens %v", tokens)
	}
	if !reflect.DeepEqual(chunks, []string{"First fact", "Second", "A paragraph on two lines"}) {
		t.Fatalf("chunks %v", chunks)
	}
	if len([]rune(long)) != 242 || !strings.HasSuffix(long, "word...") || memory.Snippet("  a \n b ") != "a b" {
		t.Fatalf("snippet %q", long)
	}
}

func TestRankUsesWorksHybridWeights(t *testing.T) {
	// score = 0.45 keyword + 0.25 cosine + 0.15 path/tag + 0.10 importance + 0.05 recency, rounded to 6 decimals.
	// Arrange
	e := entry("m1", "memories/semantic/facts/report.md", "The risk report goes to the CRO", now, "fact", "Risk")

	// Act
	hits := memory.Rank([]*store.MemoryEntry{e}, "risk report", 5, now)

	// Assert
	if len(hits) != 1 {
		t.Fatalf("hits %v", hits)
	}
	// Searchable bag: the×2 risk report goes to cro (content), the whole path (one token), fact, risk (tags).
	cosine := 3 / (math.Sqrt(2) * math.Sqrt(4+4+1+1+1+1+1+1))
	want := math.Round((0.45*1+0.25*cosine+0.15*0.5+0.10*0.85+0.05*1)*1e6) / 1e6
	c := hits[0].Components
	if hits[0].Score != want || c["keyword"] != 1 || c["path_tag"] != 0.5 || c["importance"] != 0.85 || c["recency"] != 1 {
		t.Fatalf("score %v components %v, want %v", hits[0].Score, c, want)
	}
	if hits[0].ChunkID != "memories/semantic/facts/report.md#chunk-0001" {
		t.Fatalf("chunk id %q", hits[0].ChunkID)
	}
}

func TestRankOrdersByScoreThenRecency(t *testing.T) {
	// Keyword matches outrank tag-only matches, which outrank unrelated entries (still ranked by importance and recency, like Work); equal scores put the most recent first; top_k bounds the list.
	// Arrange
	entries := []*store.MemoryEntry{
		entry("unrelated", "memories/semantic/notes/a.md", "Likes green tea", now),
		entry("tagged", "memories/semantic/notes/b.md", "Uses a Mac", now, "laptop"),
		entry("match-old", "memories/semantic/notes/c.md", "Laptop is a Mac", now.Add(-60*24*time.Hour)),
		entry("match-new", "memories/semantic/notes/d.md", "Laptop is a Mac", now),
	}

	// Act
	hits := memory.Rank(entries, "laptop", 10, now)
	top2 := memory.Rank(entries, "laptop", 2, now)
	none := memory.Rank(entries, "  ", 10, now)

	// Assert
	var ids []string
	for _, h := range hits {
		ids = append(ids, h.Entry.ID)
	}
	if !reflect.DeepEqual(ids, []string{"match-new", "match-old", "tagged", "unrelated"}) {
		t.Fatalf("order %v", ids)
	}
	if len(top2) != 2 || none != nil {
		t.Fatalf("top2 %d none %v", len(top2), none)
	}
}

func TestImportanceScoreFromTagsContentOrExplicitValue(t *testing.T) {
	// Decision/constraint tags 1.0, fact/preference/procedure 0.85, strong words 0.75, change words 0.65, else 0.35; an explicit importance wins.
	explicit := 0.2
	cases := []struct {
		tags    []string
		content string
		set     *float64
		want    float64
	}{
		{[]string{"Decision"}, "x", nil, 1},
		{[]string{"preference"}, "x", nil, 0.85},
		{nil, "We must ship on Friday", nil, 0.75},
		{nil, "Fixed the build", nil, 0.65},
		{nil, "Lunch", nil, 0.35},
		{[]string{"decision"}, "x", &explicit, 0.2},
	}
	for _, c := range cases {
		if got := memory.ImportanceScore(c.tags, c.content, c.set); got != c.want {
			t.Errorf("%v %q: %v, want %v", c.tags, c.content, got, c.want)
		}
	}
}

func TestRecencyScoreDecaysOverAMonth(t *testing.T) {
	// 1 / (1 + age_days / 30): 1 today, 0.5 after 30 days, 0 without a timestamp.
	if memory.RecencyScore(now, now) != 1 || memory.RecencyScore(now.Add(-30*24*time.Hour), now) != 0.5 || memory.RecencyScore(time.Time{}, now) != 0 {
		t.Fatal("recency")
	}
}
