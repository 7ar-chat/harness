package runtime_test

import (
	"strings"
	"testing"

	goopenai "github.com/meguminnnnnnnnn/go-openai"

	"github.com/GhaziBenDahmane/harness/agent"
	"github.com/GhaziBenDahmane/harness/compaction"
	"github.com/GhaziBenDahmane/harness/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/store"
)

func overflowErr() error {
	return &goopenai.APIError{HTTPStatusCode: 400, Code: "context_length_exceeded", Message: "This model's maximum context length is 128000 tokens."}
}

func TestProcessCompactsAndRetriesWhenContextOverflows(t *testing.T) {
	// A context-overflow error is not retried as transient: the history is compacted once (below the threshold too) and the call is retried with it.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Err: overflowErr()},
		llmtest.Turn{Chunks: llmtest.Text("## Goal\nAnalyse the 2025 sales.")},
		llmtest.Turn{Chunks: llmtest.Text("Done.")},
	)
	th, run, answer := r.longThread()

	// Act
	got := r.process(run)

	// Assert
	calls := r.model.Calls()
	if got.Status != store.StatusCompleted || len(calls) != 3 || calls[1].Messages[0].Content != compaction.Instructions {
		t.Fatalf("status %s calls %d", got.Status, len(calls))
	}
	retry := calls[2].Messages
	if len(retry) != 2 || !strings.Contains(retry[0].Content, agent.SummaryHeading) || !strings.Contains(retry[0].Content, "Analyse the 2025 sales.") ||
		retry[1].Content != "q2: now plot it" {
		t.Fatalf("retry %+v", retry)
	}
	ce := compactionEvents(r.events(run.ID))
	if len(ce) != 2 || ce[0]["phase"] != "started" || ce[1]["phase"] != "completed" || ce[1]["trigger"] != "auto" {
		t.Fatalf("compaction events %v", ce)
	}
	if sum, _ := r.st.GetThreadSummary(t.Context(), th.ID); sum.ThroughPosition != answer.Position {
		t.Fatalf("summary %+v", sum)
	}
}

func TestProcessFailsWhenContextStillOverflowsAfterCompaction(t *testing.T) {
	// A second overflow after the compaction ends the run with RUN_ERROR model_error (no second compaction).
	// Arrange
	r := newRig(t,
		llmtest.Turn{Err: overflowErr()},
		llmtest.Turn{Chunks: llmtest.Text("SUMMARY")},
		llmtest.Turn{Err: overflowErr()},
		llmtest.Turn{Chunks: llmtest.Text("unexpected")},
	)
	_, run, _ := r.longThread()

	// Act
	got := r.process(run)

	// Assert
	fin := last(r.events(run.ID))
	if got.Status != store.StatusFailed || fin["type"] != "RUN_ERROR" || fin["code"] != "model_error" || len(r.model.Calls()) != 3 {
		t.Fatalf("status %s last %v calls %d", got.Status, fin, len(r.model.Calls()))
	}
}

func TestProcessFailsOnOverflowWhenNothingCanBeCompacted(t *testing.T) {
	// An overflow on the first call of a new thread (only the user message) fails the run with model_error without a summary call.
	// Arrange
	r := newRig(t, llmtest.Turn{Err: overflowErr()}, llmtest.Turn{Chunks: llmtest.Text("unexpected")})
	_, run := r.queued(strings.Repeat("huge paste ", 100))

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	if got.Status != store.StatusFailed || last(evs)["code"] != "model_error" || len(r.model.Calls()) != 1 || len(compactionEvents(evs)) != 0 {
		t.Fatalf("status %s calls %d types %v", got.Status, len(r.model.Calls()), types(evs))
	}
}
