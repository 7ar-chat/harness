package runtime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/compaction"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/store"
)

// fetchTool returns size characters per call ({"n": k} keeps the calls distinct for the loop guard).
func fetchTool(size int) *fakeTool {
	return &fakeTool{name: "fetch", run: func(_ context.Context, args string) (string, error) {
		var in struct{ N int }
		_ = json.Unmarshal([]byte(args), &in)
		return fmt.Sprintf("page %d: ", in.N) + strings.Repeat("d", size), nil
	}}
}

func fetchCall(n int) llmtest.Turn {
	return llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, fmt.Sprintf("f%d", n), "fetch", fmt.Sprintf(`{"n":%d}`, n))}}
}

// fetchCallUsage is fetchCall reporting a prompt size.
func fetchCallUsage(n, prompt int) llmtest.Turn {
	t := fetchCall(n)
	t.Chunks[0] = llmtest.WithUsage(t.Chunks[0], prompt, 5)
	return t
}

// isSummaryCall reports a summarizer call (its system prompt is the compaction instructions).
func isSummaryCall(c llmtest.Call) bool {
	return len(c.Messages) > 0 && c.Messages[0].Content == compaction.Instructions
}

func roles(msgs []*schema.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, string(m.Role))
	}
	return out
}

// validToolOrder reports whether every tool message follows the assistant message that called it.
func validToolOrder(msgs []*schema.Message) bool {
	called := map[string]bool{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			called[tc.ID] = true
		}
		if m.Role == schema.Tool && !called[m.ToolCallID] {
			return false
		}
	}
	return true
}

const longRequest = "Write the 2025 sales report as /workspace/outputs/report.docx: one section per region, a chart per section, in French."

func TestProcessCompactsInsideALongTurnAndKeepsTheRequestVerbatim(t *testing.T) {
	// A single-request document job is compacted at a step boundary inside its turn: the request is still sent verbatim as the first message and no tool result loses its call.
	// Arrange
	turns := []llmtest.Turn{}
	for n := 1; n <= 11; n++ {
		turns = append(turns, fetchCall(n))
	}
	turns = append(turns, fetchCallUsage(12, 11500), llmtest.Turn{Chunks: llmtest.Text("## Goal\nSales report.")}, llmtest.Turn{Chunks: llmtest.Text("Report written.")})
	r := newRig(t, turns...)
	// The estimate (system prompt + tool schemas + history) stays under 75% of the window for 11 calls; the
	// prompt size reported by call 12 crosses it. Grow the window if the system prompt or the tool set grows.
	r.window, r.agent.MaxSteps = 15000, 20
	r.withTools(fetchTool(1600)) // ≈ 400 estimated tokens per result: the 30 % tail cannot hold all 12

	_, run := r.queued(longRequest)

	// Act
	got := r.process(run)

	// Assert
	calls := r.model.Calls()
	if got.Status != store.StatusCompleted || len(calls) != 14 || !isSummaryCall(calls[12]) {
		t.Fatalf("status %s calls %d", got.Status, len(calls))
	}
	final := calls[13].Messages
	if final[1].Role != schema.User || final[1].Content != longRequest || !validToolOrder(final) || final[2].Role != schema.Assistant ||
		!strings.Contains(final[0].Content, agent.SummaryHeading) || len(final) >= 26 {
		t.Fatalf("after compaction %v %q", roles(final), final[1].Content)
	}
}

func TestProcessRecoversFromAnOverflowInsideTheFirstTurn(t *testing.T) {
	// An overflow in the middle of a first turn is compacted inside the turn (request carried verbatim) and the call retried.
	// Arrange
	turns := []llmtest.Turn{}
	for n := 1; n <= 6; n++ {
		turns = append(turns, fetchCall(n))
	}
	turns = append(turns, llmtest.Turn{Err: overflowErr()}, llmtest.Turn{Chunks: llmtest.Text("SUMMARY")}, llmtest.Turn{Chunks: llmtest.Text("Done.")})
	r := newRig(t, turns...)
	r.agent.MaxSteps = 10
	r.withTools(fetchTool(1400))
	_, run := r.queued(longRequest)

	// Act
	got := r.process(run)

	// Assert
	calls := r.model.Calls()
	if got.Status != store.StatusCompleted || len(calls) != 9 || !isSummaryCall(calls[7]) {
		t.Fatalf("status %s calls %d", got.Status, len(calls))
	}
	retry := calls[8].Messages
	if retry[1].Content != longRequest || !validToolOrder(retry) || len(retry) >= len(calls[6].Messages) || !strings.Contains(retry[0].Content, "SUMMARY") {
		t.Fatalf("retry %v", roles(retry))
	}
}

func TestProcessSummarizesInChunksThatFitTheSummarizingModelsWindow(t *testing.T) {
	// With COMPACTION_MODEL's 4000-token window, the fold is summarized in several calls that each fit that window and together cover the oldest and newest messages (A-07).
	// Arrange
	r := newRig(t)
	th, first, _ := r.longThread()
	r.process(first)
	compact := r.create(th, store.NewRun{Kind: store.KindCompact})
	r.model = llmtest.New(llmtest.Turn{Chunks: llmtest.Text("## Goal\nSales analysis")})
	r.agent.Models = llmtest.Factory{M: r.model}
	r.agent.Harness.CompactionModel = "small-summarizer"
	r.agent.Harness.ContextWindow = func(m string) int {
		if m == "small-summarizer" {
			return 4000
		}
		return 128000
	}

	// Act
	got := r.process(compact)

	// Assert
	calls := r.model.Calls()
	if got.Status != store.StatusCompleted || len(calls) < 2 {
		t.Fatalf("status %s calls %d", got.Status, len(calls))
	}
	for i, c := range calls {
		if in := compaction.EstimateMessages(c.Messages); in > 4000-1000 {
			t.Fatalf("summary call %d input of %d tokens", i, in)
		}
	}
	if !strings.Contains(calls[0].Messages[1].Content, "q1: analyse the 2025 sales") || !strings.Contains(calls[len(calls)-1].Messages[1].Content, "q2: now plot it") {
		t.Fatalf("chunks do not cover the fold: first %q", calls[0].Messages[1].Content[:200])
	}
}

// scriptedFetches is a model that calls fetch n times, answers "SUMMARY" to summarizer calls and then "Done.".
func scriptedFetches(n int, prompt int) *llmtest.Model {
	k := 0
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if isSummaryCall(c) {
			return llmtest.Turn{Chunks: llmtest.Text("SUMMARY")}
		}
		if k++; k > n {
			return llmtest.Turn{Chunks: llmtest.Text("Done.")}
		}
		if prompt > 0 {
			return fetchCallUsage(k, prompt)
		}
		return fetchCall(k)
	})
}

func TestProcessKeepsARecentTailOfAboutHalfTheThreshold(t *testing.T) {
	// With a low COMPACTION_THRESHOLD (0.3 of 20000 tokens), the verbatim tail kept by a compaction is clamped to about threshold/2 (3000 tokens) so it gets back under the threshold.
	// Arrange
	r := newRig(t)
	r.model = scriptedFetches(20, 0)
	r.agent.Models = llmtest.Factory{M: r.model}
	r.window, r.agent.MaxSteps = 20000, 25
	r.agent.Harness.CompactionThreshold = 0.3
	r.withTools(fetchTool(1400))
	_, run := r.queued(longRequest)

	// Act
	got := r.process(run)

	// Assert
	calls := r.model.Calls()
	compacted := 0
	for i := 1; i < len(calls); i++ {
		if !isSummaryCall(calls[i-1]) || isSummaryCall(calls[i]) {
			continue
		}
		compacted++
		msgs := calls[i].Messages
		if msgs[1].Content != longRequest {
			t.Fatalf("request not carried: %v", roles(msgs))
		}
		if tail := compaction.EstimateMessages(msgs[2:]); tail > 3000+500 {
			t.Fatalf("verbatim tail of %d tokens after the compaction before call %d", tail, i)
		}
	}
	if got.Status != store.StatusCompleted || compacted == 0 {
		t.Fatalf("status %s compactions %d", got.Status, compacted)
	}
}

func TestProcessDoesNotRecompactUntilTheHistoryHasGrown(t *testing.T) {
	// When a compaction cannot bring the reported prompt under the threshold (small window), the next one waits until the history grew by about half the kept tail instead of summarizing every step.
	// Arrange
	r := newRig(t)
	r.model = scriptedFetches(24, 9000)
	r.agent.Models = llmtest.Factory{M: r.model}
	r.window, r.agent.MaxSteps = 10000, 30
	r.withTools(fetchTool(1400))
	_, run := r.queued(longRequest)

	// Act
	got := r.process(run)

	// Assert
	var at []int
	steps := 0
	for _, c := range r.model.Calls() {
		if isSummaryCall(c) {
			at = append(at, steps)
		} else {
			steps++
		}
	}
	if got.Status != store.StatusCompleted || len(at) == 0 {
		t.Fatalf("status %s summaries at steps %v", got.Status, at)
	}
	for i := 1; i < len(at); i++ {
		if at[i]-at[i-1] < 3 {
			t.Fatalf("summaries at steps %v", at)
		}
	}
}

func TestCancellingDuringTheSummaryEndsTheRunCancelledWithoutAFailedCompaction(t *testing.T) {
	// A cancel while the summarizer runs ends the run as cancelled: no work.compaction failed, nothing persisted as failed or dropped.
	// Arrange
	summarizing := make(chan struct{})
	r := newRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.WithUsage(llmtest.ToolCall(0, "p1", "update_plan", `{"items":[]}`), 16000, 10)}},
		llmtest.Turn{Block: true, OnCall: func() { close(summarizing) }},
	)
	r.window = 20000
	th, run, _ := r.longThread()
	done := make(chan *store.Run)
	go func() { done <- r.process(run) }()
	<-summarizing

	// Act
	_, _ = r.st.RequestCancel(context.Background(), run.ID)
	_ = r.q.PublishCancel(context.Background(), run.ID)

	// Assert
	var got *store.Run
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop")
	}
	evs := r.events(run.ID)
	fin := last(evs)
	for _, c := range compactionEvents(evs) {
		if c["phase"] == "failed" {
			t.Fatalf("compaction reported failed: %v", compactionEvents(evs))
		}
	}
	sum, _ := r.st.GetThreadSummary(context.Background(), th.ID)
	if got.Status != store.StatusCancelled || fin["type"] != "RUN_FINISHED" || fin["outcome"].(map[string]any)["type"] != "cancelled" || sum.Text != "" {
		t.Fatalf("status %s last %v summary %+v", got.Status, fin, sum)
	}
}

func TestCancellingAManualCompactionDuringTheSummaryEndsItCancelled(t *testing.T) {
	// A compact run cancelled while summarizing ends cancelled without a failed compaction or a recorded marker.
	// Arrange
	summarizing := make(chan struct{})
	r := newRig(t)
	th, first, _ := r.longThread()
	r.process(first)
	compact := r.create(th, store.NewRun{Kind: store.KindCompact})
	r.model = llmtest.New(llmtest.Turn{Block: true, OnCall: func() { close(summarizing) }})
	r.agent.Models = llmtest.Factory{M: r.model}
	done := make(chan *store.Run)
	go func() { done <- r.process(compact) }()
	<-summarizing

	// Act
	_, _ = r.st.RequestCancel(context.Background(), compact.ID)
	_ = r.q.PublishCancel(context.Background(), compact.ID)

	// Assert
	got := <-done
	evs := r.events(compact.ID)
	for _, c := range compactionEvents(evs) {
		if c["phase"] == "failed" {
			t.Fatalf("compaction reported failed: %v", compactionEvents(evs))
		}
	}
	list, _ := r.st.ListCompactions(context.Background(), th.ID)
	if got.Status != store.StatusCancelled || last(evs)["outcome"].(map[string]any)["type"] != "cancelled" || len(list) != 0 {
		t.Fatalf("status %s types %v markers %d", got.Status, types(evs), len(list))
	}
}
