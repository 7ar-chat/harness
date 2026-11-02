package memory

import (
	"sync/atomic"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

// maxRevisions bounds the history kept in an entry's metadata.
const maxRevisions = 10

// addRevision records what next replaces: the previous title and content, when, and by which thread,
// newest first under metadata["history"]. A memory that changes (a decision reversed, a command
// that no longer works) keeps what it used to say, so the change can be reviewed and undone.
func addRevision(next *store.MemoryEntry, prevTitle, prevContent string, at time.Time, thread string) {
	if next.Metadata == nil {
		next.Metadata = map[string]any{}
	}
	h, _ := next.Metadata["history"].([]any)
	rev := map[string]any{"title": prevTitle, "content": truncateRunes(prevContent, 2000), "replaced_at": at.UTC().Format(time.RFC3339)}
	if thread != "" {
		rev["thread"] = thread
	}
	h = append([]any{rev}, h...)
	if len(h) > maxRevisions {
		h = h[:maxRevisions]
	}
	next.Metadata["history"] = h
}

// serviceStats are the process-wide counters of the memory service.
type serviceStats struct {
	recallRuns, recallItems, recallChars, recallPinned          atomic.Int64
	searches, searchHits                                        atomic.Int64
	extractRuns, extractProposed, extractSkipped, extractErrors atomic.Int64
	embedded, embedErrors                                       atomic.Int64
}

// Stats is a snapshot of the counters (since the process started).
type Stats struct {
	RecallRuns      int64 `json:"recallRuns"`
	RecallItems     int64 `json:"recallItems"`
	RecallChars     int64 `json:"recallChars"`
	RecallPinned    int64 `json:"recallPinned"`
	Searches        int64 `json:"searches"`
	SearchHits      int64 `json:"searchHits"`
	ExtractRuns     int64 `json:"extractRuns"`
	ExtractProposed int64 `json:"extractProposed"`
	ExtractSkipped  int64 `json:"extractSkipped"`
	ExtractErrors   int64 `json:"extractErrors"`
	Embedded        int64 `json:"embedded"`
	EmbedErrors     int64 `json:"embedErrors"`
}

// Stats returns the counters.
func (s *Service) Stats() Stats {
	st := &s.stats
	return Stats{st.recallRuns.Load(), st.recallItems.Load(), st.recallChars.Load(), st.recallPinned.Load(),
		st.searches.Load(), st.searchHits.Load(), st.extractRuns.Load(), st.extractProposed.Load(),
		st.extractSkipped.Load(), st.extractErrors.Load(), st.embedded.Load(), st.embedErrors.Load()}
}
