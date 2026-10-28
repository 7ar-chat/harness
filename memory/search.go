package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GhaziBenDahmane/harness/store"
)

// SearchIndexVersion is reported with every search result (Work's SEARCH_INDEX_VERSION).
const SearchIndexVersion = 1

// Hybrid ranking weights of Work's _score_memory_chunk.
const (
	weightKeyword    = 0.45
	weightSemantic   = 0.25
	weightPathTag    = 0.15
	weightImportance = 0.10
	weightRecency    = 0.05
)

// SnippetChars bounds a search result snippet.
const SnippetChars = 240

var tokenPattern = regexp.MustCompile(`[A-Za-z0-9_./:-]+`)

// Tokenize is Work's _tokenize: ASCII words (with _ . / : -), lowercased.
func Tokenize(text string) []string {
	found := tokenPattern.FindAllString(text, -1)
	out := make([]string, 0, len(found))
	for _, t := range found {
		if strings.TrimSpace(t) != "" {
			out = append(out, strings.ToLower(t))
		}
	}
	return out
}

func counter(tokens []string) map[string]int {
	c := make(map[string]int, len(tokens))
	for _, t := range tokens {
		c[t]++
	}
	return c
}

func set(tokens []string) map[string]bool {
	s := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		s[t] = true
	}
	return s
}

// Cosine is the cosine similarity of two bags of words: Work's "simple"
// embedding backend (term-frequency vectors, no remote embedding model).
func Cosine(left, right map[string]int) float64 {
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	num := 0.0
	for t, v := range left {
		num += float64(v * right[t])
	}
	norm := func(m map[string]int) float64 {
		s := 0.0
		for _, v := range m {
			s += float64(v * v)
		}
		return math.Sqrt(s)
	}
	l, r := norm(left), norm(right)
	if l == 0 || r == 0 {
		return 0
	}
	return num / (l * r)
}

// RecencyScore is 1 / (1 + age_days / 30).
func RecencyScore(ts, now time.Time) float64 {
	if ts.IsZero() {
		return 0
	}
	age := now.Sub(ts).Hours() / 24
	if age < 0 {
		age = 0
	}
	return 1 / (1 + age/30)
}

var (
	strongWords = regexp.MustCompile(`\b(decided|must|cannot|required|prefer|always|never)\b`)
	changeWords = regexp.MustCompile(`\b(discovered|fixed|implemented|updated)\b`)
)

// ImportanceScore is Work's _importance_score from tags and content, unless
// the entry has an explicit importance (set by the user or metadata.importance).
func ImportanceScore(tags []string, content string, explicit *float64) float64 {
	if explicit != nil {
		return *explicit
	}
	ts := map[string]bool{}
	for _, t := range tags {
		ts[strings.ToLower(t)] = true
	}
	any := func(names ...string) bool {
		for _, n := range names {
			if ts[n] {
				return true
			}
		}
		return false
	}
	c := strings.ToLower(content)
	switch {
	case any("decision", "decisions", "constraint", "constraints"):
		return 1
	case any("fact", "facts", "preference", "preferences", "procedure", "procedures"):
		return 0.85
	case strongWords.MatchString(c):
		return 0.75
	case changeWords.MatchString(c):
		return 0.65
	}
	return 0.35
}

// SplitChunks is Work's _split_memory_content: paragraphs and list items, headings dropped.
func SplitChunks(content string) []string {
	var chunks, para []string
	flush := func() {
		if len(para) > 0 {
			chunks = append(chunks, strings.TrimSpace(strings.Join(para, " ")))
			para = nil
		}
	}
	for _, raw := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "#"):
			flush()
		case strings.HasPrefix(line, "-") || strings.HasPrefix(line, "*"):
			flush()
			chunks = append(chunks, strings.TrimSpace(strings.TrimLeft(line, "-* ")))
		default:
			para = append(para, line)
		}
	}
	flush()
	out := chunks[:0]
	for _, c := range chunks {
		if strings.TrimSpace(c) != "" {
			out = append(out, c)
		}
	}
	return out
}

// Snippet collapses whitespace and cuts at SnippetChars ("..." appended).
func Snippet(content string) string {
	compact := strings.Join(strings.Fields(content), " ")
	if utf8.RuneCountInString(compact) <= SnippetChars {
		return compact
	}
	return strings.TrimRight(string([]rune(compact)[:SnippetChars-1]), " \t\n") + "..."
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Hit is one ranked chunk of a search.
type Hit struct {
	Entry      *store.MemoryEntry
	ChunkID    string
	Chunk      string
	Score      float64
	Components map[string]float64
}

// entryTime is the timestamp of an entry for ranking and payloads.
func entryTime(e *store.MemoryEntry) time.Time {
	if !e.UpdatedAt.IsZero() {
		return e.UpdatedAt
	}
	return e.CreatedAt
}

// Rank scores every chunk of entries against query with Work's hybrid formula
// (0.45 keyword + 0.25 cosine + 0.15 path/tag + 0.10 importance + 0.05
// recency) and returns the topK best (ties: most recent first).
func Rank(entries []*store.MemoryEntry, query string, topK int, now time.Time) []Hit {
	qTokens := Tokenize(query)
	if len(qTokens) == 0 {
		return nil
	}
	if topK < 1 {
		topK = 1
	}
	if topK > MaxTopK {
		topK = MaxTopK
	}
	qCounter, qSet := counter(qTokens), set(qTokens)
	var hits []Hit
	for _, e := range entries {
		pathTokens := Tokenize(e.Path)
		tagTokens := make([]string, 0, len(e.Tags))
		for _, t := range e.Tags {
			tagTokens = append(tagTokens, strings.ToLower(t))
		}
		for i, chunk := range SplitChunks(e.Content) {
			contentTokens := Tokenize(chunk)
			searchable := append(append(append([]string{}, contentTokens...), pathTokens...), tagTokens...)
			if len(searchable) == 0 {
				continue
			}
			contentSet, pathTagSet := set(contentTokens), set(append(append([]string{}, pathTokens...), tagTokens...))
			kw, pt := 0, 0
			for t := range qSet {
				if contentSet[t] {
					kw++
				}
				if pathTagSet[t] {
					pt++
				}
			}
			keyword := float64(kw) / float64(len(qSet))
			pathTag := float64(pt) / float64(len(qSet))
			semantic := Cosine(qCounter, counter(searchable))
			recency := RecencyScore(entryTime(e), now)
			importance := ImportanceScore(e.Tags, chunk, e.Importance)
			score := weightKeyword*keyword + weightSemantic*semantic + weightPathTag*pathTag + weightImportance*importance + weightRecency*recency
			// Like Work, every chunk with a positive score ranks (importance and
			// recency alone make one): the best memories come back even without a word in common.
			if score <= 0 {
				continue
			}
			hits = append(hits, Hit{Entry: e, ChunkID: fmt.Sprintf("%s#chunk-%04d", e.Path, i+1), Chunk: chunk, Score: round6(score),
				Components: map[string]float64{"keyword": round6(keyword), "semantic": round6(semantic), "path_tag": round6(pathTag),
					"importance": round6(importance), "recency": round6(recency)}})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return entryTime(hits[i].Entry).After(entryTime(hits[j].Entry))
	})
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits
}
