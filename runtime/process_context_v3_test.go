package runtime_test

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/agent"
	"github.com/GhaziBenDahmane/harness/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/tools"
)

func TestProcessCompactsAtWindowMinusReserve(t *testing.T) {
	// Without COMPACTION_THRESHOLD, compaction triggers at window − reserve (max output + 20k), well below 0.75 × window.
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: llmtest.Text("## Goal\nAnalyse the 2025 sales.")}, llmtest.Turn{Chunks: llmtest.Text("Here is the plot")})
	r.window = 20000
	r.agent.Harness.CompactionThreshold = 0
	r.agent.Harness.OutputReserve = func(string) int { return 14000 } // trigger at 6000 tokens (0.75 × window = 15000)
	_, run, _ := r.longThread()

	// Act
	got := r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	ce := compactionEvents(r.events(run.ID))
	if got.Status != store.StatusCompleted || len(ce) != 2 || ce[1]["phase"] != "completed" {
		t.Fatalf("status %s compaction %v", got.Status, ce)
	}
}

func TestProcessReportsACancelledCompaction(t *testing.T) {
	// A run cancelled while its compaction summarizes emits work.compaction phase "cancelled" after "started", then ends cancelled (A-20).
	// Arrange
	r := newRig(t, llmtest.Turn{Block: true})
	r.window = 10000 // the 30000-character answer alone passes 0.75 × window
	_, run, _ := r.longThread()
	done := make(chan *store.Run)
	go func() { done <- r.process(run) }()
	waitFor(t, "compaction started", func() bool { return contains(r.liveTypes(run.ID), "CUSTOM") })

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
	checkStrictOrder(t, evs)
	ce := compactionEvents(evs)
	if got.Status != store.StatusCancelled || len(ce) != 2 || ce[0]["phase"] != "started" || ce[1]["phase"] != "cancelled" || ce[1]["trigger"] != "auto" {
		t.Fatalf("status %s compaction %v", got.Status, ce)
	}
}

// dumpTool returns a 2000-character command-like result that ends with its exit code (not valid JSON, like a cut output).
func dumpTool() *fakeTool {
	return &fakeTool{name: "dump", run: func(_ context.Context, args string) (string, error) {
		return `{"stdout":"` + args + strings.Repeat("a", 2000) + `","stderr":"boom","exitCode":1}`, nil
	}}
}

func dumpCall(n int) llmtest.Turn {
	return llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, fmt.Sprintf("d%d", n), "dump", fmt.Sprintf(`{"n":%d}`, n))}}
}

func TestProcessMasksOldToolResultsInTheModelInput(t *testing.T) {
	// With COMPACTION_MASK_FROM_FRACTION=0 (mask from the first step), tool results older than the newest COMPACTION_KEEP_RECENT_TOOL_RESULTS become a one-line stub in the model input, in batches (the boundary moves by min(4, keep) results), so the request prefix is identical between batches; the stub never says to re-run; stored results and events stay full (A-24).
	// Arrange
	r := newRig(t, dumpCall(1), dumpCall(2), dumpCall(3), dumpCall(4), dumpCall(5), dumpCall(6), llmtest.Turn{Chunks: llmtest.Text("done")})
	r.agent.MaxSteps = 8
	r.agent.Harness.KeepRecentToolResults = 2
	r.agent.Harness.MaskFromFraction = agent.MaskFromSetting(0)
	r.withTools(dumpTool())
	th, run := r.queued("dump everything")

	// Act
	got := r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	calls := r.model.Calls()
	if got.Status != store.StatusCompleted || len(calls) != 7 {
		t.Fatalf("status %s calls %d", got.Status, len(calls))
	}
	results := func(c llmtest.Call) []string {
		var out []string
		for _, m := range c.Messages {
			if m.Role == schema.Tool {
				out = append(out, m.Content)
			}
		}
		return out
	}
	stub := "[dump result, 2.0 KB, exit 1 — hidden to save context]"
	stubs := func(rs []string) int {
		n := 0
		for _, c := range rs {
			if c == stub {
				n++
			} else if !strings.HasSuffix(c, `"exitCode":1}`) {
				t.Fatalf("result %q", c)
			}
		}
		return n
	}
	// Results seen by calls 3..6: 3, 4, 5, 6 → masked 0, 2, 2, 4 (keep 2, batch 2).
	for k, want := range map[int]int{3: 0, 4: 2, 5: 2, 6: 4} {
		if n := stubs(results(calls[k])); n != want {
			t.Fatalf("call %d: %d stubs, want %d", k, n, want)
		}
	}
	prev, cur := calls[4].Messages, calls[5].Messages
	for j := range prev {
		if !sameMessage(prev[j], cur[j]) {
			t.Fatalf("message %d changed within a masking batch", j)
		}
	}
	for _, m := range r.messages(th.ID) {
		if m.Role == store.RoleTool && !strings.HasSuffix(m.Content, `"exitCode":1}`) {
			t.Fatalf("stored result masked: %q", m.Content)
		}
	}
	for _, e := range find(r.events(run.ID), "TOOL_CALL_RESULT") {
		if !strings.HasSuffix(e["content"].(string), `"exitCode":1}`) {
			t.Fatalf("event masked: %v", e["content"])
		}
	}
}

func TestProcessKeepsTheTailOfOldToolOutputsFromEarlierRuns(t *testing.T) {
	// A tool output from an earlier turn keeps its head and tail (stderr, exitCode) in the next runs' model input, not only its head (A-24).
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: llmtest.Text("ok")})
	ctx := context.Background()
	th, first := r.queued("q1")
	result := `{"stdout":"` + strings.Repeat("a", 5000) + `","stderr":"Traceback: boom","exitCode":2}`
	_ = r.st.AppendMessages(ctx,
		&store.Message{ThreadID: th.ID, RunID: first.ID, Role: store.RoleAssistant, ToolCalls: []store.ToolCall{{ID: "x1", Type: "function", Function: store.ToolCallFunction{Name: "execute_command", Arguments: `{"command":"python a.py"}`}}}},
		&store.Message{ThreadID: th.ID, RunID: first.ID, Role: store.RoleTool, ToolCallID: "x1", Content: result},
		&store.Message{ThreadID: th.ID, RunID: first.ID, Role: store.RoleAssistant, Content: "It failed."})
	_, _ = r.st.ClaimRun(ctx, first.ID, "rt")
	_ = r.st.FinishRun(ctx, first.ID, "rt", store.StatusCompleted, "", "")
	second := r.queuedOn(th, "q2")
	_ = r.st.AppendMessages(ctx, &store.Message{ThreadID: th.ID, RunID: second.ID, Role: store.RoleAssistant, Content: "Fixed."})
	_, _ = r.st.ClaimRun(ctx, second.ID, "rt")
	_ = r.st.FinishRun(ctx, second.ID, "rt", store.StatusCompleted, "", "")
	run := r.queuedOn(th, "q3")

	// Act
	r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	var got string
	for _, m := range r.model.Calls()[0].Messages {
		if m.Role == schema.Tool {
			got = m.Content
		}
	}
	if len(got) >= len(result) || !strings.Contains(got, `"exitCode":2}`) || !strings.Contains(got, "Traceback: boom") {
		t.Fatalf("old result in the model input: %q", got)
	}
}

func TestProcessSpillsALargeToolOutputToAFile(t *testing.T) {
	// An output over AGENT_TOOL_OUTPUT_SPILL_CHARS is written in full to a file of /workspace/.work/tool-output/ of the running sandbox; the model gets its head, tail, path and a paging hint, so nothing is silently dropped (A-04).
	// Arrange
	big := "HEAD-MARK " + strings.Repeat("row,1,2,3\n", 5000) + " TAIL-MARK"
	r := newRig(t, llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "c0", "execute_command", `{"command":"true"}`)}},
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "c1", "dump", `{}`)}}, llmtest.Turn{Chunks: llmtest.Text("ok")})
	r.withTools(&fakeTool{name: "dump", run: func(context.Context, string) (string, error) { return big, nil }})
	r.agent.Harness.Spill = tools.SandboxSpiller{Sandboxes: r.mgr}
	th, run := r.queued("dump the table")

	// Act
	got := r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	content := find(r.events(run.ID), "TOOL_CALL_RESULT")[1]["content"].(string)
	spilled := regexp.MustCompile(`/workspace/\.work/tool-output/c1-[0-9a-f]{8}\.txt`).FindString(content)
	full, ok := r.file(spilled)
	if got.Status != store.StatusCompleted || !ok || full != big {
		t.Fatalf("status %s spilled to %q: %v (%d chars)", got.Status, spilled, ok, len(full))
	}
	if !strings.HasPrefix(content, "HEAD-MARK") || !strings.HasSuffix(content, "TAIL-MARK") ||
		!strings.Contains(content, "read_file") || len(content) > 13000 {
		t.Fatalf("model view (%d chars): %q", len(content), content[:300])
	}
	if msgs := r.messages(th.ID); msgs[4].Content != content {
		t.Fatalf("stored result differs from the model view")
	}
}

func assistantReasoning(c llmtest.Call) []string {
	var out []string
	for _, m := range c.Messages {
		if m.Role == schema.Assistant {
			out = append(out, m.ReasoningContent)
		}
	}
	return out
}

func TestProcessPassesReasoningBackWithinAndAcrossRuns(t *testing.T) {
	// For LLM_REASONING_PASSBACK_MODELS, assistant messages carry their reasoning_content on the next steps and in later runs (stored reasoning rows); other models never get it (A-10).
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.Reasoning("think 1"), llmtest.ToolCall(0, "c1", "list_files", `{}`)}},
		llmtest.Turn{Chunks: []*schema.Message{llmtest.Reasoning("think 2"), {Role: schema.Assistant, Content: "listed"}}},
		llmtest.Turn{Chunks: llmtest.Text("again")},
	)
	r.agent.Harness.ReasoningPassback = func(m string) bool { return m == "deepseek-test" }
	th, _ := r.st.CreateThread(t.Context(), "alice", "t")
	first := r.create(th, store.NewRun{Model: "deepseek-test", Message: store.Message{Role: store.RoleUser, Content: "list"}})
	r.process(first)
	second := r.create(th, store.NewRun{Model: "deepseek-test", Message: store.Message{Role: store.RoleUser, Content: "list again"}})
	other := newRig(t, llmtest.Turn{Chunks: []*schema.Message{llmtest.Reasoning("think"), llmtest.ToolCall(0, "c1", "list_files", `{}`)}}, llmtest.Turn{Chunks: llmtest.Text("ok")})
	_, plain := other.queued("list")

	// Act
	r.process(second)
	other.process(plain)

	// Assert
	checkStrictOrder(t, r.events(first.ID))
	calls := r.model.Calls()
	if got := assistantReasoning(calls[1]); len(got) != 1 || got[0] != "think 1" {
		t.Fatalf("within the run: %q", got)
	}
	if got := assistantReasoning(calls[2]); len(got) != 2 || got[0] != "think 1" || got[1] != "think 2" {
		t.Fatalf("across runs: %q", got)
	}
	if got := assistantReasoning(other.model.Calls()[1]); len(got) != 1 || got[0] != "" {
		t.Fatalf("model without passback got %q", got)
	}
}

func sameMessage(a, b *schema.Message) bool {
	if a.Role != b.Role || a.Content != b.Content || a.ToolCallID != b.ToolCallID || len(a.ToolCalls) != len(b.ToolCalls) {
		return false
	}
	for i := range a.ToolCalls {
		if a.ToolCalls[i].ID != b.ToolCalls[i].ID || a.ToolCalls[i].Function.Arguments != b.ToolCalls[i].Function.Arguments {
			return false
		}
	}
	return true
}

func TestProcessKeepsTheRequestPrefixStableAndSendsAPromptCacheKey(t *testing.T) {
	// Each step's request starts with the previous step's request byte for byte (system prompt first, history never rewritten), and calls carry the thread id as prompt_cache_key for models that accept it (2.7).
	// Arrange
	r := newRig(t, dumpCall(1), dumpCall(2), dumpCall(3), llmtest.Turn{Chunks: llmtest.Text("done")})
	r.withTools(dumpTool())
	r.agent.Harness.PromptCacheKey = func(m string) bool { return m == "gpt-test" }
	th, run := r.queued("dump")

	// Act
	got := r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	calls := r.model.Calls()
	if got.Status != store.StatusCompleted || len(calls) != 4 {
		t.Fatalf("status %s calls %d", got.Status, len(calls))
	}
	for i := 1; i < len(calls); i++ {
		prev, cur := calls[i-1].Messages, calls[i].Messages
		for j := range prev {
			if !sameMessage(prev[j], cur[j]) {
				t.Fatalf("call %d rewrote message %d of the previous request", i, j)
			}
		}
	}
	for i, c := range calls {
		if c.PromptCacheKey != th.ID {
			t.Fatalf("call %d prompt_cache_key %q", i, c.PromptCacheKey)
		}
	}
}

// fakeDeliverables is the C2 deliverables source.
type fakeDeliverables []agent.Deliverable

func (f fakeDeliverables) Deliverables(context.Context, *store.Run) []agent.Deliverable { return f }

func TestProcessRehydratesPlanSkillsAndDeliverablesAfterACompaction(t *testing.T) {
	// After a compaction folds a loaded skill away, the next calls get a context message right after the system prompt with the current plan, the skill's text, and the deliverables with their last checker status (2.5).
	// Arrange
	skill := "SKILL docx: always run the finish gate before answering. " + strings.Repeat("z", 12000)
	ok := true
	r := newRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "s1", "load_skill", `{"name":"docx"}`)}},
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "p1", "update_plan", `{"items":[{"id":"1","title":"Write the report","status":"completed"}]}`)}},
		llmtest.Turn{Chunks: []*schema.Message{llmtest.WithUsage(llmtest.ToolCall(0, "d1", "dump", `{}`), 16000, 10)}},
		llmtest.Turn{Chunks: llmtest.Text("## Goal\nWrite the report.")},
		llmtest.Turn{Chunks: llmtest.Text("Report written.")},
	)
	r.window = 20000
	r.agent.SpillChars = 100000
	r.withTools(&fakeTool{name: "load_skill", run: func(context.Context, string) (string, error) { return skill, nil }},
		&fakeTool{name: "dump", run: func(context.Context, string) (string, error) { return strings.Repeat("y", 23000), nil }}) // the tail cannot hold the skill
	r.agent.Harness.Plans = r.st
	r.agent.Harness.Deliverables = fakeDeliverables{{Path: "/workspace/outputs/r.docx", Checker: "inspect_docx", OK: &ok, Summary: "12 checks passed"}}
	_, run := r.queued("write the report as docx")

	// Act
	got := r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	calls := r.model.Calls()
	if got.Status != store.StatusCompleted || len(calls) != 5 || !isSummaryCall(calls[3]) {
		t.Fatalf("status %s calls %d", got.Status, len(calls))
	}
	final := calls[4].Messages
	ctxMsg := final[1]
	for _, want := range []string{"SKILL docx: always run the finish gate", "Write the report", "completed", "/workspace/outputs/r.docx", "inspect_docx", "12 checks passed"} {
		if ctxMsg.Role != schema.User || !strings.Contains(ctxMsg.Content, want) {
			t.Fatalf("context message lacks %q: %+v", want, ctxMsg)
		}
	}
	if final[2].Content != "write the report as docx" || strings.Contains(ctxMsg.Content, "write the report as docx") {
		t.Fatalf("the request is not kept once, verbatim: %q / %q", final[2].Content, ctxMsg.Content)
	}
}
