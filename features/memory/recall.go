package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/prompts"
)

// Recall budgets (characters).
const (
	// PinnedBudget bounds the always-on block: pinned entries are in every run of the memory.
	PinnedBudget = 2500
	// PinnedEntryMax bounds one pinned entry in that block (the rest is one memory_get away).
	PinnedEntryMax = 1200
	// RelevantItems is how many query-matched entries are recalled.
	RelevantItems = 6
	// MinRelevance is the least hybrid score of a recalled entry.
	MinRelevance = 0.12
	// MaxRecallTotal bounds the whole block.
	MaxRecallTotal = 6500
)

// IsPinned reports an entry that is recalled in every run: metadata pinned=true, or the tag "pinned".
func IsPinned(e *store.MemoryEntry) bool {
	if v, ok := e.Metadata["pinned"].(bool); ok && v {
		return true
	}
	for _, t := range e.Tags {
		if strings.EqualFold(strings.TrimSpace(t), "pinned") {
			return true
		}
	}
	return false
}

// RecallInfo says what a recall contained (stats, tests, the UI).
type RecallInfo struct {
	Pinned   []string // entry ids
	Relevant []string
	Fallback bool
	Chars    int
}

// RecallBlock builds the memory the run starts with: the pinned entries of every layer, then the
// entries that match the query (the user's last message) by words and, with an embedder, meaning;
// with no query hit it falls back to the most recent decisions, facts and constraints. "" when empty.
func (s *Service) RecallBlock(ctx context.Context, layers []Layer, query string) (string, RecallInfo) {
	var info RecallInfo
	type tagged struct {
		e     *store.MemoryEntry
		layer string
	}
	var pinned, rest []tagged
	byLayer := map[string][]*store.MemoryEntry{}
	layerOf := map[string]string{}
	for _, l := range layers {
		entries, err := s.st.ListMemoryEntries(ctx, l.Key, store.MemoryActive)
		if err != nil {
			continue
		}
		byLayer[l.Key] = entries
		for _, e := range entries {
			layerOf[e.ID] = l.Name
			if IsPinned(e) {
				pinned = append(pinned, tagged{e, l.Name})
			} else {
				rest = append(rest, tagged{e, l.Name})
			}
		}
	}
	var lines []string
	used := 0
	// Pinned entries first: shared before personal, then most important, then most recent.
	sort.SliceStable(pinned, func(i, j int) bool {
		a, b := pinned[i], pinned[j]
		if a.layer != b.layer {
			return a.layer != "me"
		}
		ia, ib := ImportanceScore(a.e.Tags, "", a.e.Importance), ImportanceScore(b.e.Tags, "", b.e.Importance)
		if ia != ib {
			return ia > ib
		}
		return entryTime(a.e).After(entryTime(b.e))
	})
	for _, p := range pinned {
		body := strings.TrimSpace(p.e.Content)
		if utf8.RuneCountInString(body) > PinnedEntryMax {
			body = truncateRunes(body, PinnedEntryMax) + " … (memory_get " + p.e.ID + " for the rest)"
		}
		block := fmt.Sprintf("[%s] %s\n%s", p.layer, firstNonEmpty(p.e.Title, p.e.Path), body)
		if used+utf8.RuneCountInString(block) > PinnedBudget {
			break
		}
		lines = append(lines, "- "+strings.ReplaceAll(block, "\n", "\n  "))
		used += utf8.RuneCountInString(block)
		info.Pinned = append(info.Pinned, p.e.ID)
	}
	// Entries that match what the user just asked.
	if q := strings.TrimSpace(query); q != "" && len(rest) > 0 {
		cands := make([]*store.MemoryEntry, 0, len(rest))
		for _, r := range rest {
			cands = append(cands, r.e)
		}
		opts := RankOptions{}
		if qv := s.embedQuery(ctx, q); qv != nil {
			vecs := map[string][]float32{}
			for _, l := range layers {
				var inLayer []*store.MemoryEntry
				for _, e := range byLayer[l.Key] {
					if !IsPinned(e) {
						inLayer = append(inLayer, e)
					}
				}
				for id, v := range s.vectors(ctx, l.Key, inLayer) {
					vecs[id] = v
				}
			}
			opts = RankOptions{QueryVec: qv, Vecs: vecs}
		}
		seen := map[string]bool{}
		for _, h := range RankWith(cands, q, RelevantItems*3, s.now(), opts) {
			if len(info.Relevant) >= RelevantItems || seen[h.Entry.ID] {
				continue
			}
			if h.Score < MinRelevance || !(h.Components["semantic"] >= 0.35 && opts.QueryVec != nil || sharesMeaningfulWord(q, h)) {
				continue
			}
			line := fmt.Sprintf("- [%s] %s: %s", layerOf[h.Entry.ID], h.Entry.Path, Snippet(strings.Join(strings.Fields(h.Chunk), " ")))
			if used+utf8.RuneCountInString(line) > MaxRecallTotal {
				break
			}
			lines = append(lines, line)
			used += utf8.RuneCountInString(line)
			seen[h.Entry.ID] = true
			info.Relevant = append(info.Relevant, h.Entry.ID)
		}
	}
	// Nothing matched: the latest decisions, facts and constraints of the primary memory.
	if len(info.Relevant) == 0 && len(layers) > 0 {
		var plain []*store.MemoryEntry
		for _, e := range byLayer[layers[0].Key] {
			if !IsPinned(e) {
				plain = append(plain, e)
			}
		}
		for _, it := range RecallItems(plain, MaxRecallItems) {
			lines = append(lines, "- "+strings.Join(strings.Fields(it), " "))
			info.Fallback = true
		}
	}
	if len(lines) == 0 {
		return "", info
	}
	text := agent.FenceData(prompts.RecallTag, strings.Join(lines, "\n"))
	for utf8.RuneCountInString(text) > MaxRecallTotal+200 && len(lines) > 1 {
		lines = lines[:len(lines)-1]
		text = agent.FenceData(prompts.RecallTag, strings.Join(lines, "\n"))
	}
	info.Chars = utf8.RuneCountInString(text)
	return text, info
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// lastUserText is the user's latest message of the thread (not a steer): what recall searches with.
func (s *Service) lastUserText(ctx context.Context, threadID string) string {
	if s.Messages == nil {
		return ""
	}
	msgs, err := s.Messages(ctx, threadID)
	if err != nil {
		return ""
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if m := msgs[i]; m.Role == store.RoleUser && !m.IsSteer() && strings.TrimSpace(m.Content) != "" {
			return truncateRunes(m.Content, 1000)
		}
	}
	return ""
}

var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`the and for are was were with that this from have has had not but you your our can could would should
		what when where which who why how does did doing done into about than then them they their there here also just only some any all
		use used using make made get got let lets please need want like see look new old
		les des une est sont avec pour dans que qui pas par sur aux comment pourquoi quoi quel quelle faire fait peut`) {
		m[w] = true
	}
	return m
}()

// stem is a light normalization so that "releases" matches "release".
func stem(w string) string {
	for _, suf := range []string{"ing", "es", "s", "ed"} {
		if len(w) > len(suf)+3 && strings.HasSuffix(w, suf) {
			return strings.TrimSuffix(w, suf)
		}
	}
	return w
}

// sharesMeaningfulWord reports whether the query and the hit have a content word in common (stop
// words and very short words do not count): the gate that keeps unrelated entries out of the
// prompt, since the hybrid score alone also rewards importance and recency.
func sharesMeaningfulWord(query string, h Hit) bool {
	have := map[string]bool{}
	for _, t := range Tokenize(h.Chunk + " " + h.Entry.Path + " " + h.Entry.Title + " " + strings.Join(h.Entry.Tags, " ")) {
		have[stem(t)] = true
	}
	for _, t := range Tokenize(query) {
		if len(t) >= 3 && !stopwords[t] && have[stem(t)] {
			return true
		}
	}
	return false
}
