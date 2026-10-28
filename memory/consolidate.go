package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/GhaziBenDahmane/harness/store"
)

// Consolidation of the memory (deduplication strategy and the cleanup task), run periodically by the runtime sweeper. The other
// Other strategies need data the harness does not have: episodic summaries
// (memories/episodic/ run journals), promotion (runs/*/summary.md candidates)
// and indexing (tags/entities/timeline YAML files, which Postgres replaces
// with queries on the entries).

// semanticPath matches the entries deduplicated across entries: the semantic
// memory files (decisions.md…) and the per-entry files of the semantic folders.
var semanticPath = regexp.MustCompile(`^((?:shared/)?memories/semantic)/(decisions|facts|constraints|preferences|procedures|notes)(?:\.md|/[^/]+)$`)

// aggregatedPath matches the files whose lines Work deduplicates (its
// DeduplicationStrategy files_to_check, under memories/semantic and
// shared/memories/semantic); the content of other entries is never rewritten.
var aggregatedPath = regexp.MustCompile(`^(?:shared/)?memories/semantic/(?:decisions|facts|constraints)\.md$`)

var (
	// listItem is a markdown list item marker ("- ", "* ", "+ ", "1. ", "1) "):
	// Work's [-*\d.]+ also took leading digits ("2024 budget") and "**bold**" lines for markers.
	listItem      = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+`)
	headerLine    = regexp.MustCompile(`^#{1,6}\s`)
	fenceLine     = regexp.MustCompile("^\\s{0,3}(```+|~~~+)")
	trailingParen = regexp.MustCompile(`\s*\([^)]*\)\s*$`)
	trailingPunct = regexp.MustCompile(`[.,:;!?]+$`)
)

// NormalizeItem is Work's _normalize_entry: no list marker, lowercase,
// collapsed whitespace, no trailing parenthetical note or punctuation.
func NormalizeItem(item string) string {
	t := strings.ToLower(listItem.ReplaceAllString(strings.TrimSpace(item), ""))
	t = strings.Join(strings.Fields(t), " ")
	t = trailingParen.ReplaceAllString(t, "")
	return trailingPunct.ReplaceAllString(strings.TrimSpace(t), "")
}

// lineRange is a list item: lines[start:end].
type lineRange struct{ start, end int }

// listItems finds the list items of a markdown text (Work's _parse_entries:
// an item and its continuation lines, up to a blank line, a header or the next
// item), skipping fenced code blocks; paragraphs are not items.
func listItems(lines []string) []lineRange {
	var items []lineRange
	cur := -1
	closeItem := func(end int) {
		if cur >= 0 {
			items = append(items, lineRange{cur, end})
			cur = -1
		}
	}
	fence := ""
	for i, line := range lines {
		if m := fenceLine.FindStringSubmatch(line); m != nil {
			switch {
			case fence == "":
				closeItem(i)
				fence = m[1]
			case m[1][0] == fence[0] && len(m[1]) >= len(fence):
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		trimmed := strings.TrimSpace(line)
		switch {
		case listItem.MatchString(trimmed):
			closeItem(i)
			cur = i
		case trimmed == "" || headerLine.MatchString(trimmed):
			closeItem(i)
		}
	}
	closeItem(len(lines))
	return items
}

// DedupeContent removes the duplicate list items of a markdown file (Work's
// _find_duplicates: same text without the marker, or same normalized text),
// keeping the first; only the removed items' lines go (blank lines, headers,
// paragraphs and fenced code blocks are kept as they are). It returns the
// content and the count removed.
func DedupeContent(content string) (string, int) {
	lines := strings.Split(content, "\n")
	seenHash, seenNorm := map[string]bool{}, map[string]bool{}
	drop := map[int]bool{}
	removed := 0
	for _, it := range listItems(lines) {
		text := strings.TrimSpace(strings.Join(lines[it.start:it.end], "\n"))
		h, n := sha(listItem.ReplaceAllString(text, "")), NormalizeItem(text)
		if seenHash[h] || seenNorm[n] {
			removed++
			for i := it.start; i < it.end; i++ {
				drop[i] = true
			}
			continue
		}
		seenHash[h], seenNorm[n] = true, true
	}
	if removed == 0 {
		return content, 0
	}
	kept := make([]string, 0, len(lines)-len(drop))
	for i, line := range lines {
		if !drop[i] {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n"), removed
}

// Report is the outcome of one consolidation.
type Report struct {
	Users int
	// Processed counts the semantic entries checked, ItemsRemoved the duplicate
	// lines removed inside them, Duplicates the entries soft-deleted as copies
	// of an older one, Skipped the entries changed (or deleted) since they were
	// listed (left as they are), Purged the soft-deleted entries past the retention.
	Processed    int
	ItemsRemoved int
	Duplicates   int
	Skipped      int
	Purged       int
}

// Consolidator is the periodic memory job.
type Consolidator struct {
	Store store.MemoryStore
	// Retention: soft-deleted entries are purged this long after deletion (≤ 0: never).
	Retention time.Duration
	Now       func() time.Time
}

func (c Consolidator) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

// Run implements the runtime job.
func (c Consolidator) Run(ctx context.Context) error {
	r, err := c.Consolidate(ctx)
	if r.ItemsRemoved+r.Duplicates+r.Skipped+r.Purged > 0 {
		slog.Info("memory: consolidated", "users", r.Users, "processed", r.Processed, "items_removed", r.ItemsRemoved,
			"duplicates", r.Duplicates, "skipped", r.Skipped, "purged", r.Purged)
	}
	return err
}

// Consolidate deduplicates every user's semantic memory, then purges old
// deleted entries.
func (c Consolidator) Consolidate(ctx context.Context) (Report, error) {
	var r Report
	var errs []error
	users, err := c.Store.ListMemoryUsers(ctx)
	if err != nil {
		return r, errors.Join(append(errs, err)...)
	}
	for _, login := range users {
		if ctx.Err() != nil {
			return r, ctx.Err()
		}
		if err := c.dedupeUser(ctx, login, &r); err != nil {
			errs = append(errs, fmt.Errorf("user %s: %w", login, err))
		}
		r.Users++
	}
	if c.Retention > 0 {
		n, err := c.Store.PurgeDeletedMemoryEntries(ctx, c.now().Add(-c.Retention))
		r.Purged = n
		if err != nil {
			errs = append(errs, err)
		}
	}
	return r, errors.Join(errs...)
}

// dedupeUser works on a snapshot of the user's entries: every write is
// guarded by the version read (store.UpdateMemoryEntry), so an entry the user
// or the agent changed or deleted meanwhile is skipped, never overwritten nor
// resurrected. Rewriting the content keeps the status and deleted_at read.
func (c Consolidator) dedupeUser(ctx context.Context, login string, r *Report) error {
	entries, err := c.Store.ListMemoryEntries(ctx, login, store.MemoryActive)
	if err != nil {
		return err
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].CreatedAt.Before(entries[j].CreatedAt)
		}
		return entries[i].ID < entries[j].ID
	})
	now := c.now()
	// save reports false when the entry changed since it was listed.
	save := func(e *store.MemoryEntry) (*store.MemoryEntry, bool, error) {
		out, err := c.Store.UpdateMemoryEntry(ctx, e, store.MemoryLimits{})
		if errors.Is(err, store.ErrMemoryConflict) || errors.Is(err, store.ErrNotFound) {
			r.Skipped++
			return nil, false, nil
		}
		return out, err == nil, err
	}
	kept := map[string]string{} // group + whitespace-normalized content hash → kept entry id
	for _, e := range entries {
		m := semanticPath.FindStringSubmatch(e.Path)
		if m == nil {
			continue
		}
		r.Processed++
		if aggregatedPath.MatchString(e.Path) {
			if content, removed := DedupeContent(e.Content); removed > 0 {
				e.Content, e.ContentHash, e.UpdatedAt, e.ContentUpdatedAt = content, sha(content), now, now
				saved, ok, err := save(e)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
				e = saved
				r.ItemsRemoved += removed
			}
		}
		// Separate entries are copies only when identical up to whitespace (the
		// line normalization above is for the items of one aggregated file).
		key := strings.Join([]string{e.Scope, e.Repository, m[1], m[2], sha(strings.Join(strings.Fields(e.Content), " "))}, "\x00")
		first, dup := kept[key]
		if !dup {
			kept[key] = e.ID
			continue
		}
		e.Status, e.DeletedAt, e.UpdatedAt = store.MemoryDeleted, &now, now
		e.Metadata = cloneMeta(e.Metadata)
		e.Metadata["consolidation"] = map[string]any{"reason": "duplicate", "duplicate_of": first, "at": now.Format(time.RFC3339)}
		_, ok, err := save(e)
		if err != nil {
			return err
		}
		if ok {
			r.Duplicates++
		}
	}
	return nil
}
