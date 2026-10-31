package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/compaction"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
)

// longThread stores a finished first run (a request and a 30000-character answer) and returns the thread's next run.
func (r *runRig) longThread(next string) (*store.Run, *store.Message) {
	r.t.Helper()
	ctx := context.Background()
	first := r.userRun("q1: analyse the 2025 sales")
	answer := &store.Message{ThreadID: r.th.ID, RunID: first.ID, Role: store.RoleAssistant, Content: strings.Repeat("x", 30000)}
	if err := r.st.AppendMessages(ctx, answer); err != nil {
		r.t.Fatal(err)
	}
	r.finish(first)
	return r.userRun(next), answer
}

func (r *runRig) compactionPhases() []string {
	var out []string
	for _, e := range r.events(agui.Custom + ":" + agui.CustomCompaction) {
		v := e["value"].(map[string]any)
		out = append(out, fmt.Sprint(v["phase"], "/", v["trigger"]))
	}
	return out
}

func compactionRig(t *testing.T, turns ...llmtest.Turn) *runRig {
	t.Helper()
	r := newRunRig(t, turns...)
	r.a.Harness.ContextWindow = func(string) int { return 20000 }
	r.a.Harness.CompactionThreshold = 0.75
	r.a.Harness.Summaries = r.st
	r.a.Harness.Compactions = r.st
	return r
}

func TestRunSummarizesOldContextWhenThePromptPassesTheThreshold(t *testing.T) {
	// Arrange
	r := compactionRig(t,
		llmtest.Turn{Chunks: []*ai.Message{llmtest.WithUsage(llmtest.ToolCall(0, "p1", "update_plan", `{"items":[{"id":"1","title":"Plot","status":"completed"}]}`), 16000, 10)}},
		textTurn("## Goal\nAnalyse the 2025 sales."),
		textTurn("Here is the plot"))
	r.withTools(tool("update_plan", "saved"))
	run, answer := r.longThread("q2: now plot it")

	// Act
	_, err := r.run(run)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(r.compactionPhases()); got != "[started/auto completed/auto]" {
		t.Fatalf("phases %s", got)
	}
	calls := r.model.Calls()
	if len(calls) != 3 || calls[1].Messages[0].Content != compaction.Instructions || !strings.Contains(calls[1].Messages[1].Content, "q1: analyse the 2025 sales") {
		t.Fatalf("summarizer call: %d calls", len(calls))
	}
	final := calls[2].Messages
	if !strings.Contains(final[0].Content, SummaryHeading) || !strings.Contains(final[0].Content, "Analyse the 2025 sales.") {
		t.Fatalf("the summary is not in the system prompt: %.200s", final[0].Content)
	}
	if !strings.Contains(final[1].Content, RehydrationTag) || !strings.Contains(final[1].Content, "[completed] Plot") {
		t.Fatalf("restored context: %q", final[1].Content)
	}
	sum, _ := r.st.GetThreadSummary(context.Background(), r.th.ID)
	if sum.ThroughPosition != answer.Position || !strings.Contains(sum.Text, "Analyse the 2025 sales.") {
		t.Fatalf("stored summary %+v (answer at %d)", sum, answer.Position)
	}
	if cs, _ := r.st.ListCompactions(context.Background(), r.th.ID); len(cs) != 1 || cs[0].Trigger != TriggerAuto {
		t.Fatalf("recorded compactions %+v", cs)
	}
}

func TestRunCountsTheSummarizersUsageUnderItsOwnModelName(t *testing.T) {
	// Arrange
	summary := llmtest.Turn{Chunks: []*ai.Message{llmtest.WithUsage(&ai.Message{Content: "## Goal\nsales"}, 700, 90)}}
	r := compactionRig(t,
		llmtest.Turn{Chunks: []*ai.Message{llmtest.WithUsage(llmtest.ToolCall(0, "p1", "update_plan", `{"items":[]}`), 16000, 10)}},
		summary, textTurn("done"))
	r.withTools(tool("update_plan", "saved"))
	r.a.Harness.CompactionModel = "cheap-summarizer"
	r.a.Harness.SupportsReasoning = func(m string) bool { return m == "cheap-summarizer" }
	run, _ := r.longThread("q2")

	// Act
	res, err := r.run(run)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]agui.Usage{}
	for _, u := range res.Usage {
		by[u.Model] = u
	}
	if u := by["cheap-summarizer"]; u.InputTokens != 700 || u.OutputTokens != 90 {
		t.Fatalf("usage by model %+v", by)
	}
	if by["gpt-test"].InputTokens != 16000 {
		t.Fatalf("main usage %+v", by["gpt-test"])
	}
	if c := r.model.Calls()[1]; c.ReasoningEffort != "low" {
		t.Fatalf("summarizer effort %q", c.ReasoningEffort)
	}
}

func TestRunDropsTheOldContextWhenSummarizingFails(t *testing.T) {
	// Arrange
	r := compactionRig(t,
		llmtest.Turn{Chunks: []*ai.Message{llmtest.WithUsage(llmtest.ToolCall(0, "p1", "update_plan", `{"items":[]}`), 16000, 10)}},
		llmtest.Turn{Err: errors.New("summarizer down")}, textTurn("ok"))
	r.withTools(tool("update_plan", "saved"))
	run, _ := r.longThread("q2")

	// Act
	_, err := r.run(run)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(r.compactionPhases()); got != "[started/auto failed/auto]" {
		t.Fatalf("phases %s", got)
	}
	final := r.model.Calls()[2].Messages
	if !strings.Contains(final[0].Content, "2 earlier messages were dropped") {
		t.Fatalf("no drop marker: %.300s", final[0].Content)
	}
	if sum, _ := r.st.GetThreadSummary(context.Background(), r.th.ID); sum.Text != "" {
		t.Fatalf("dropped context persisted as a summary: %+v", sum)
	}
	if cs, _ := r.st.ListCompactions(context.Background(), r.th.ID); len(cs) != 0 {
		t.Fatalf("a failed compaction was recorded: %+v", cs)
	}
}

func TestRunCompactsAndRetriesOnceWhenTheProviderReportsAContextOverflow(t *testing.T) {
	// Arrange
	r := compactionRig(t, llmtest.Turn{Err: statusErr{400, "status code: 400 context_length_exceeded"}}, textTurn("## Goal\nsales"), textTurn("fits now"))
	run, _ := r.longThread("q2")

	// Act
	_, err := r.run(run)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	calls := r.model.Calls()
	if len(calls) != 3 || calls[1].Messages[0].Content != compaction.Instructions {
		t.Fatalf("%d calls", len(calls))
	}
	if a := r.lastMessage(store.RoleAssistant); a.Content != "fits now" {
		t.Fatalf("answer %q", a.Content)
	}
}

func TestRunFailsWhenTheOverflowPersistsAfterCompaction(t *testing.T) {
	// Arrange
	overflow := llmtest.Turn{Err: statusErr{400, "status code: 400 context_length_exceeded"}}
	r := compactionRig(t, overflow, textTurn("## Goal\nsales"), overflow, overflow)
	run, _ := r.longThread("q2")

	// Act
	_, err := r.run(run)

	// Assert
	if errCode(t, err) != agui.CodeModelError {
		t.Fatalf("err %v", err)
	}
}

func TestRunDoesNotCompactAHistoryThatFits(t *testing.T) {
	// Arrange
	r := compactionRig(t, textTurn("short answer"))

	// Act
	_, err := r.do("small question")

	// Assert
	if err != nil || len(r.compactionPhases()) != 0 || len(r.model.Calls()) != 1 {
		t.Fatalf("err %v phases %v calls %d", err, r.compactionPhases(), len(r.model.Calls()))
	}
}

func TestRunSendsOnlyTheMessagesAfterTheStoredSummary(t *testing.T) {
	// Arrange
	r := compactionRig(t, textTurn("ok"))
	run, answer := r.longThread("q2: now plot it")
	if err := r.st.SetThreadSummary(context.Background(), r.th.ID, store.ThreadSummary{Text: "The user analyses 2025 sales.", ThroughPosition: answer.Position}); err != nil {
		t.Fatal(err)
	}

	// Act
	_, err := r.run(run)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	msgs := r.model.Calls()[0].Messages
	if len(msgs) != 2 || !strings.Contains(msgs[0].Content, "The user analyses 2025 sales.") || msgs[1].Content != "q2: now plot it" {
		t.Fatalf("messages %+v", msgs)
	}
}

func TestCompactRunSummarizesTheWholeThreadAndRecordsItManually(t *testing.T) {
	// Arrange
	r := compactionRig(t, textTurn("## Goal\nsales analysis"))
	run, _ := r.longThread("")
	r.finish(run) // the thread's open run
	compactRun := r.newRun(store.NewRun{Kind: store.KindCompact})

	// Act
	res, err := r.run(compactRun)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(r.compactionPhases()); got != "[started/manual completed/manual]" {
		t.Fatalf("phases %s", got)
	}
	if len(res.Usage) != 1 || len(r.model.Calls()) != 1 {
		t.Fatalf("usage %+v calls %d", res.Usage, len(r.model.Calls()))
	}
	if sum, _ := r.st.GetThreadSummary(context.Background(), r.th.ID); !strings.Contains(sum.Text, "sales analysis") {
		t.Fatalf("summary %+v", sum)
	}
	if cs, _ := r.st.ListCompactions(context.Background(), r.th.ID); len(cs) != 1 || cs[0].Trigger != TriggerManual {
		t.Fatalf("compactions %+v", cs)
	}
}

func TestCompactRunOfAnEmptyThreadStillReportsCompletion(t *testing.T) {
	// Arrange
	r := compactionRig(t)
	run := r.newRun(store.NewRun{Kind: store.KindCompact})

	// Act
	_, err := r.run(run)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	ev := r.events(agui.Custom + ":" + agui.CustomCompaction)
	if len(ev) != 1 || ev[0]["value"].(map[string]any)["phase"] != "completed" || ev[0]["value"].(map[string]any)["summarizedMessages"] != 0 {
		t.Fatalf("events %v", ev)
	}
}

func TestCompactionThresholdFollowsTheModelsOutputReserve(t *testing.T) {
	for name, tc := range map[string]struct {
		configured float64
		window     int
		reserve    func(string) int
		want       float64
	}{
		"configured":        {0.6, 100000, nil, 0.6},
		"out of range":      {1.5, 100000, nil, 0.64},
		"default reserve":   {0, 100000, nil, 0.64},
		"model reserve":     {0, 100000, func(string) int { return 20000 }, 0.8},
		"reserve too large": {0, 100000, func(string) int { return 90000 }, 0.3},
		"tiny reserve":      {0, 100000, func(string) int { return 1 }, 0.95},
		"zero reserve":      {0, 100000, func(string) int { return 0 }, 0.64},
		"unknown window":    {0, 0, nil, 0.75},
	} {
		t.Run(name, func(t *testing.T) {
			st := newState(5)
			st.a.Harness.CompactionThreshold, st.a.Harness.OutputReserve = tc.configured, tc.reserve
			st.h.window = tc.window
			if got := st.threshold(); fmt.Sprintf("%.2f", got) != fmt.Sprintf("%.2f", tc.want) {
				t.Fatalf("threshold %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWindowFallsBackToTheDefaultForUnknownModels(t *testing.T) {
	a := &Agent{}
	if a.window("m") <= 0 {
		t.Fatal("no default window")
	}
	a.Harness.ContextWindow = func(m string) int {
		if m == "big" {
			return 1_000_000
		}
		return 0
	}
	if a.window("big") != 1_000_000 || a.window("other") != a.window("") {
		t.Fatalf("windows %d %d", a.window("big"), a.window("other"))
	}
}

func TestSummaryBudgetLeavesRoomForTheInstructionsAndTheSummary(t *testing.T) {
	st := newState(5)
	st.a.Harness.ContextWindow = func(m string) int {
		if m == "small" {
			return 2000
		}
		return 128000
	}
	big := st.summaryBudget()
	st.a.Harness.CompactionModel = "small"
	small := st.summaryBudget()
	if small != 1000 || big < 100000 || big >= 128000 {
		t.Fatalf("budgets big %d small %d", big, small)
	}
}

func TestSummarizerModelFallsBackToTheRunsModelWhenTheConfiguredOneIsUnavailable(t *testing.T) {
	// Arrange
	r := newRunRig(t)
	st := newState(5)
	st.a = r.a
	st.a.Models = failingFactory{}
	st.a.Harness.CompactionModel = "missing"
	base := r.model

	// Act
	m := st.summarizerModel(context.Background(), base)

	// Assert
	if m != ai.ChatModel(base) || st.summarizerName() != "m" {
		t.Fatalf("summarizer %v name %q", m, st.summarizerName())
	}
	if again := st.summarizerModel(context.Background(), nil); again != m {
		t.Fatal("the choice is not kept for the run")
	}
}

func TestSubagentUsageJoinsTheRunTotalsUnderItsOwnName(t *testing.T) {
	// Arrange
	st := newState(5)
	em := &toolEmitter{st: st}

	// Act
	em.AddSubagentUsage("gpt-mini", 100, 20, 30)
	em.AddSubagentUsage("gpt-mini", 50, 5, 0)
	usages := st.usages()

	// Assert
	if len(usages) != 2 || usages[0].Model != "m" {
		t.Fatalf("usages %+v", usages)
	}
	u := usages[1]
	if u.Model != "gpt-mini (sub-agent)" || u.InputTokens != 150 || u.OutputTokens != 25 || u.TotalTokens != 175 || u.CachedInputTokens != 30 {
		t.Fatalf("sub-agent usage %+v", u)
	}
}

func TestAddModelUsageKeepsTheRunsModelApartFromAuxiliaryOnes(t *testing.T) {
	// Arrange
	st := newState(5)
	msg := func(in, out int) *ai.Message {
		return llmtest.WithUsage(&ai.Message{}, in, out)
	}

	// Act
	st.addModelUsage("m", msg(10, 1))
	st.addModelUsage("aux", msg(7, 3))
	st.addModelUsage("aux", msg(1, 1))
	st.addModelUsage("aux", nil)
	st.addModelUsage("aux", &ai.Message{})

	// Assert
	if st.usage.InputTokens != 10 {
		t.Errorf("main usage %+v", st.usage)
	}
	if u := st.h.extraUsage["aux"]; u == nil || u.InputTokens != 8 || u.OutputTokens != 4 {
		t.Errorf("aux usage %+v", u)
	}
}

func TestUsageOfReadsWhatAModelCallReported(t *testing.T) {
	msg := llmtest.WithUsage(&ai.Message{}, 100, 40)
	msg.ResponseMeta.Usage.CompletionTokenDetails.ReasoningTokens = 25
	got := UsageOf("gpt", msg)
	if got.Model != "gpt" || got.InputTokens != 100 || got.OutputTokens != 40 || got.TotalTokens != 140 || got.ReasoningTokens != 25 {
		t.Fatalf("%+v", got)
	}
	for _, m := range []*ai.Message{nil, {}, {ResponseMeta: &ai.ResponseMeta{}}} {
		if u := UsageOf("gpt", m); u.Model != "gpt" || u.TotalTokens != 0 {
			t.Errorf("no usage: %+v", u)
		}
	}
}
