package memory

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/GhaziBenDahmane/harness/store"
)

// legacyBackfillKey flags the facts migration 0010 copied from work_memories
// (set by migration 0013, removed by BackfillLegacy).
const legacyBackfillKey = "legacy_backfill"

const legacyTitleChars = 120

var sqlWhitespace = regexp.MustCompile(`\s+`)

// legacyTitle is the title migration 0010 gave a former fact:
// left(btrim(regexp_replace(split_part(text, E'\n', 1), '\s+', ' ', 'g')), 120).
func legacyTitle(text string) string {
	first := strings.SplitN(text, "\n", 2)[0]
	t := strings.Trim(sqlWhitespace.ReplaceAllString(first, " "), " ")
	if r := []rune(t); len(r) > legacyTitleChars {
		t = string(r[:legacyTitleChars])
	}
	return t
}

// BackfillLegacy normalizes the facts migrated from work_memories like any
// entry written today: secrets redacted with the write policy (content and
// title), and the title derived from the content (DeriveTitle) when it is still
// the one the migration computed, so later edits re-derive it. Timestamps are
// kept. An entry changed meanwhile is skipped (still flagged, retried by the
// next run). It returns the count normalized.
func BackfillLegacy(ctx context.Context, st store.MemoryStore) (int, error) {
	entries, err := st.ListMemoryEntriesToBackfill(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, e := range entries {
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		content, _ := RedactText(e.Content)
		if e.Title == legacyTitle(e.Content) {
			e.Title = DeriveTitle(content)
		} else {
			e.Title, _ = RedactText(e.Title)
		}
		if content != e.Content {
			e.Content, e.ContentHash = content, sha(content)
		}
		e.Metadata = cloneMeta(e.Metadata)
		delete(e.Metadata, legacyBackfillKey)
		_, err := st.UpdateMemoryEntry(ctx, e, store.MemoryLimits{})
		switch {
		case errors.Is(err, store.ErrMemoryConflict), errors.Is(err, store.ErrNotFound):
		case err != nil:
			errs = append(errs, err)
		default:
			n++
		}
	}
	return n, errors.Join(errs...)
}
