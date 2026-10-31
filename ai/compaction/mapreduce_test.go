package compaction_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/compaction"
)

func TestSummaryCoversEveryFoldedMessage(t *testing.T) {
	// A fold bigger than the summarizer's budget is summarized chunk by chunk (each call merging the previous summary), so every folded message reaches a summary (A-07).
	// Arrange
	var msgs []*ai.Message
	for i := 0; i < 290; i++ {
		u := ai.UserMessage(fmt.Sprintf("msg-%d: %s", 2*i, strings.Repeat("figures for the region ", 20)))
		compaction.MarkRequest(u)
		msgs = append(msgs, u, ai.AssistantMessage(fmt.Sprintf("msg-%d: %s", 2*i+1, strings.Repeat("noted and checked ", 20)), nil))
	}
	var transcripts, previous []string
	summarize := func(_ context.Context, prev, transcript string) (string, error) {
		previous = append(previous, prev)
		transcripts = append(transcripts, transcript)
		return fmt.Sprintf("## Goal\nsummary %d", len(transcripts)), nil
	}

	// Act
	res, err := compaction.Compact(context.Background(), msgs, compaction.Options{
		Window: 1_000_000, Manual: true, Summarize: summarize, SummaryBudget: 6000,
	})

	// Assert
	if err != nil || res.Action != compaction.ActionSummarized || res.Folded != 580 || len(transcripts) < 2 {
		t.Fatalf("err %v action %s folded %d calls %d", err, res.Action, res.Folded, len(transcripts))
	}
	all := strings.Join(transcripts, "\n")
	for i := 0; i < 580; i++ {
		if !strings.Contains(all, fmt.Sprintf("msg-%d:", i)) {
			t.Fatalf("message %d reached no summary chunk (%d chunks)", i, len(transcripts))
		}
	}
	for k := 1; k < len(previous); k++ {
		if previous[k] != fmt.Sprintf("## Goal\nsummary %d", k) {
			t.Fatalf("chunk %d did not merge the previous summary: %q", k, previous[k])
		}
	}
	if !strings.HasPrefix(res.Summary, fmt.Sprintf("## Goal\nsummary %d", len(transcripts))) {
		t.Fatalf("summary %q", res.Summary)
	}
}
