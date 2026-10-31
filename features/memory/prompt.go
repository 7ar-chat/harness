package memory

import (
	"context"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/prompts"
)

// MaxRecallChars bounds the whole recall block (heading and fences included):
// MaxRecallItems lines of at most a 512-char path plus a SnippetChars line fit.
const MaxRecallChars = 4000

// PriorityRecallPaths are recalled first (Work's PRIORITY_RECALL_PATHS).
var PriorityRecallPaths = []string{
	"shared/memories/semantic/decisions.md",
	"shared/memories/semantic/facts.md",
	"shared/memories/semantic/constraints.md",
	"memories/semantic/decisions.md",
	"memories/semantic/facts.md",
	"memories/semantic/constraints.md",
}

var typedRecallPath = regexp.MustCompile(`^(shared/)?memories/semantic/(decisions|facts|constraints|preferences|procedures|notes)/[^/]+\.md$`)

// RecallItems is Work's collect_recall_items without a query: the lines of the
// priority files, then of the typed semantic entries, most recently updated
// first, as "path: line" (at most max).
func RecallItems(entries []*store.MemoryEntry, max int) []string {
	var items []string
	appendLines := func(e *store.MemoryEntry) {
		for _, raw := range strings.Split(e.Content, "\n") {
			if len(items) >= max {
				return
			}
			line := strings.TrimSpace(raw)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if strings.HasPrefix(line, "-") || strings.HasPrefix(line, "*") {
				line = strings.TrimSpace(strings.TrimLeft(line, "-* "))
			}
			if line != "" {
				// Like Work's recall of search hits ("path: snippet"), a line is cut to the snippet length.
				items = append(items, e.Path+": "+Snippet(line))
			}
		}
	}
	byPath := map[string]*store.MemoryEntry{}
	var typed []*store.MemoryEntry
	for _, e := range entries {
		if e.Status != store.MemoryActive {
			continue
		}
		byPath[e.Path] = e
		if typedRecallPath.MatchString(e.Path) {
			typed = append(typed, e)
		}
	}
	for _, p := range PriorityRecallPaths {
		if e := byPath[p]; e != nil {
			appendLines(e)
		}
		if len(items) >= max {
			return items
		}
	}
	sort.SliceStable(typed, func(i, j int) bool {
		a, b := entryTime(typed[i]), entryTime(typed[j])
		if !a.Equal(b) {
			return a.After(b)
		}
		return typed[i].Path > typed[j].Path
	})
	for _, e := range typed {
		appendLines(e)
		if len(items) >= max {
			break
		}
	}
	return items
}

// PromptSection adds the memory policy (prompts.MemoryPolicy) when the user's
// memory is enabled; the recalled entries go to the dynamic context
// (RecallSection). Pending and deleted entries are never recalled.
func PromptSection(svc *Service) agent.PromptSection {
	return func(ctx context.Context, in agent.Input) string {
		if !prompts.Has(in.Variant, prompts.Memory) {
			return ""
		}
		if !svc.enabledFor(ctx, in) {
			return ""
		}
		if svc.SharedMemory(ctx, in.Run.UserLogin, in.Run.ThreadID) {
			return prompts.ProjectMemoryPolicy
		}
		return prompts.MemoryPolicy
	}
}

// RecallSection is the dynamic-context part of memory:
// the recalled active entries fenced as <user_memories> data.
func RecallSection(svc *Service) agent.PromptSection {
	return func(ctx context.Context, in agent.Input) string {
		if !svc.o.RecallInjection || !svc.enabledFor(ctx, in) {
			return ""
		}
		layers := svc.Layers(ctx, in.Run.UserLogin, in.Run.ThreadID)
		text, info := svc.RecallBlock(ctx, layers, svc.lastUserText(ctx, in.Run.ThreadID))
		svc.stats.recallRuns.Add(1)
		svc.stats.recallItems.Add(int64(len(info.Pinned) + len(info.Relevant)))
		svc.stats.recallPinned.Add(int64(len(info.Pinned)))
		svc.stats.recallChars.Add(int64(info.Chars))
		slog.Debug("memory: recall", "run_id", in.Run.ID, "pinned", len(info.Pinned), "relevant", len(info.Relevant), "fallback", info.Fallback)
		return text
	}
}

// RecallContext renders recall items fenced as <user_memories>, dropping the
// items that would make it longer than MaxRecallChars ("" without items).
func RecallContext(items []string) string {
	overhead := utf8.RuneCountInString(agent.FenceData(prompts.RecallTag, ""))
	var lines []string
	size := overhead
	for _, it := range items {
		line := "- " + strings.Join(strings.Fields(it), " ")
		n := utf8.RuneCountInString(line) + 1
		if size+n > MaxRecallChars {
			break
		}
		lines = append(lines, line)
		size += n
	}
	if len(lines) == 0 {
		return ""
	}
	return agent.FenceData(prompts.RecallTag, strings.Join(lines, "\n"))
}

func (svc *Service) enabledFor(ctx context.Context, in agent.Input) bool {
	enabled, err := svc.st.MemoryEnabled(ctx, svc.Key(ctx, in.Run.UserLogin, in.Run.ThreadID))
	if err != nil {
		slog.Warn("memory: reading the memory setting failed", "run_id", in.Run.ID, "error", err)
		return false
	}
	return enabled
}
