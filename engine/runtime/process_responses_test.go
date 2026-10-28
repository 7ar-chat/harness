package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/engine/compaction"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/engine/llm/responses"
	"github.com/GhaziBenDahmane/harness/store"
)

// Stream R: models on the Responses API carry their encrypted reasoning items
// across tool steps and runs (docs/harness-v3.md "R" block).

// newResponsesRig is a rig whose run model goes through the real Responses
// client and the fake Responses server, with a "calc" tool and no title or goal
// hook (their calls would take scripted turns).
func newResponsesRig(t *testing.T, turns ...llmtest.Turn) *rig {
	t.Helper()
	r := newRig(t, turns...)
	r.strictOrder()
	r.agent.Models = llmtest.Factory{M: r.model, Responses: true}
	r.opts.Hooks = nil
	r.withTools(&fakeTool{name: "calc", run: func(context.Context, string) (string, error) { return "391", nil }})
	return r
}

// inputKinds lists a request's input items: "type[:role][:id or call_id]".
func inputKinds(req map[string]any) []string {
	input, _ := req["input"].([]any)
	out := make([]string, 0, len(input))
	for _, raw := range input {
		it, _ := raw.(map[string]any)
		k, _ := it["type"].(string)
		if role, ok := it["role"].(string); ok {
			k += ":" + role
		}
		if id, ok := it["id"].(string); ok {
			k += ":" + id
		}
		if id, ok := it["call_id"].(string); ok {
			k += ":" + id
		}
		out = append(out, k)
	}
	return out
}

func reasoningIDs(req map[string]any) []string {
	var out []string
	for _, k := range inputKinds(req) {
		if id, ok := strings.CutPrefix(k, "reasoning:"); ok {
			out = append(out, id)
		}
	}
	return out
}

// storedItems are the reasoning item ids persisted on the thread's assistant messages, in order.
func (r *rig) storedItems(threadID string) [][]string {
	var out [][]string
	for _, m := range r.messages(threadID) {
		if m.Role != store.RoleAssistant {
			continue
		}
		var ids []string
		for _, it := range responses.UnmarshalItems(m.ReasoningItems) {
			ids = append(ids, it.ID)
		}
		out = append(out, ids)
	}
	return out
}

func TestProcessSendsReasoningItemsBackBeforeTheirToolCalls(t *testing.T) {
	// Within a run, the reasoning item of a tool step is sent back right before its function_call, followed by the function_call_output.
	// Arrange
	r := newResponsesRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "c1", "calc", `{"expr":"17*23"}`)}},
		llmtest.Turn{Chunks: llmtest.Text("391")},
	)
	th, run := r.queued("17*23?")

	// Act
	got := r.process(run)

	// Assert
	reqs := r.model.Requests()
	stored := r.storedItems(th.ID)
	if got.Status != store.StatusCompleted || len(reqs) != 2 || len(stored) != 2 || len(stored[0]) != 1 || len(stored[1]) != 1 {
		t.Fatalf("status %s requests %d stored %v", got.Status, len(reqs), stored)
	}
	kinds := strings.Join(inputKinds(reqs[1]), " ")
	want := "message:system message:user reasoning:" + stored[0][0] + " function_call:c1 function_call_output:c1"
	if kinds != want {
		t.Fatalf("second request input\n got %s\nwant %s", kinds, want)
	}
}

func TestProcessCarriesReasoningItemsIntoTheNextRun(t *testing.T) {
	// A later run resends every reasoning item of the earlier runs, in order, each before the output it produced (persisted with its assistant message).
	// Arrange
	r := newResponsesRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "c1", "calc", `{"expr":"17*23"}`)}},
		llmtest.Turn{Chunks: llmtest.Text("391")},
		llmtest.Turn{Chunks: llmtest.Text("782")},
	)
	th, first := r.queued("17*23?")
	r.process(first)
	second := r.queuedOn(th, "and twice that?")

	// Act
	got := r.process(second)

	// Assert
	reqs := r.model.Requests()
	stored := r.storedItems(th.ID)
	if got.Status != store.StatusCompleted || len(reqs) != 3 || len(stored) != 3 {
		t.Fatalf("status %s requests %d stored %v", got.Status, len(reqs), stored)
	}
	kinds := strings.Join(inputKinds(reqs[2]), " ")
	want := "message:system message:user reasoning:" + stored[0][0] + " function_call:c1 function_call_output:c1 reasoning:" + stored[1][0] +
		" message:assistant message:user"
	if kinds != want {
		t.Fatalf("next run input\n got %s\nwant %s", kinds, want)
	}
}

func TestProcessDropsReasoningItemsWithTheirCompactedTurn(t *testing.T) {
	// A compaction that folds an earlier turn drops its reasoning items with it, while the items of the kept steps still precede their calls.
	// Arrange
	script := []llmtest.Turn{
		{Chunks: []*schema.Message{llmtest.ToolCall(0, "c1", "calc", `{"expr":"17*23"}`)}},
		{Chunks: llmtest.Text(strings.Repeat("x", 60000))},
		{Chunks: []*schema.Message{llmtest.ToolCall(0, "c2", "calc", `{"expr":"391*2"}`)}},
		{Chunks: llmtest.Text("782")},
	}
	r := newResponsesRig(t)
	r.model = llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Messages) > 0 && c.Messages[0].Content == compaction.Instructions {
			return llmtest.Turn{Chunks: llmtest.Text("## Goal\nCompute.")}
		}
		next := script[0]
		script = script[1:]
		return next
	})
	r.agent.Models = llmtest.Factory{M: r.model, Responses: true}
	th, first := r.queued("17*23?")
	r.process(first)
	r.window = 20000
	second := r.queuedOn(th, "and twice that?")

	// Act
	got := r.process(second)

	// Assert
	reqs := r.model.Requests()
	stored := r.storedItems(th.ID)
	ce := compactionEvents(r.events(second.ID))
	if got.Status != store.StatusCompleted || len(stored) != 4 || len(ce) != 2 || ce[1]["phase"] != "completed" {
		t.Fatalf("status %s stored %v compaction %v", got.Status, stored, ce)
	}
	kinds := strings.Join(inputKinds(reqs[len(reqs)-1]), " ")
	want := "message:system message:user reasoning:" + stored[2][0] + " function_call:c2 function_call_output:c2"
	if kinds != want {
		t.Fatalf("request after the compaction\n got %s\nwant %s", kinds, want)
	}
}

func TestProcessRetriesOnceWithoutRejectedReasoningItems(t *testing.T) {
	// Reasoning items the provider rejects (invalid_encrypted_content, e.g. after a key change) are dropped and the call is re-sent once without them; the run succeeds.
	// Arrange
	r := newResponsesRig(t, llmtest.Turn{Chunks: llmtest.Text("fine")})
	th, first := r.queued("hello")
	_ = r.st.AppendMessages(context.Background(), &store.Message{ThreadID: th.ID, RunID: first.ID, Role: store.RoleAssistant, Content: "Hi!",
		ReasoningItems: responses.MarshalItems([]responses.ReasoningItem{{ID: "rs_forged", EncryptedContent: "gAAAA-not-ours", Model: "gpt-test"}})})
	_, _ = r.st.ClaimRun(context.Background(), first.ID, "rt")
	_ = r.st.FinishRun(context.Background(), first.ID, "rt", store.StatusCompleted, "", "")
	second := r.queuedOn(th, "how are you?")

	// Act
	got := r.process(second)

	// Assert
	reqs := r.model.Requests()
	if got.Status != store.StatusCompleted || len(reqs) != 2 || strings.Join(reasoningIDs(reqs[0]), ",") != "rs_forged" || len(reasoningIDs(reqs[1])) != 0 ||
		len(find(r.events(second.ID), "CUSTOM:work.retry")) != 0 {
		t.Fatalf("status %s requests %d first %v second %v", got.Status, len(reqs), inputKinds(reqs[0]), inputKinds(reqs[1]))
	}
}

func TestProcessSendsReasoningItemsOnlyToTheirModel(t *testing.T) {
	// After a model change, the items of the previous model are not sent to the new one.
	// Arrange
	r := newResponsesRig(t, llmtest.Turn{Chunks: llmtest.Text("first")}, llmtest.Turn{Chunks: llmtest.Text("second")})
	th, first := r.queued("hello")
	r.process(first)
	second := r.create(th, store.NewRun{Model: "gpt-other", Message: store.Message{Role: store.RoleUser, Content: "again"}})

	// Act
	got := r.process(second)

	// Assert
	reqs := r.model.Requests()
	if got.Status != store.StatusCompleted || len(reqs) != 2 || len(reasoningIDs(reqs[0])) != 0 || len(reasoningIDs(reqs[1])) != 0 ||
		strings.Join(inputKinds(reqs[1]), " ") != "message:system message:user message:assistant message:user" {
		t.Fatalf("status %s requests %v", got.Status, reqs)
	}
}

func TestProcessResponsesRequestCarriesTheRunSettings(t *testing.T) {
	// A Responses call is stateless (store:false, encrypted reasoning included) and carries the run's reasoning effort, the thread's prompt_cache_key and parallel tool calls; its usage (cached and reasoning tokens) reaches work.usage.
	// Arrange
	answer := llmtest.WithCachedUsage(&schema.Message{Role: schema.Assistant, Content: "done"}, 1200, 1000, 50)
	answer.ResponseMeta.Usage.CompletionTokensDetails.ReasoningTokens = 30
	r := newResponsesRig(t, llmtest.Turn{Chunks: []*schema.Message{answer}})
	r.agent.Harness.PromptCacheKey = func(string) bool { return true }
	th, _ := r.st.CreateThread(t.Context(), "alice", "t")
	run := r.create(th, store.NewRun{Model: "gpt-reasoning", ReasoningEffort: "high", Message: store.Message{Role: store.RoleUser, Content: "think"}})

	// Act
	got := r.process(run)

	// Assert
	reqs := r.model.Requests()
	if got.Status != store.StatusCompleted || len(reqs) != 1 {
		t.Fatalf("status %s requests %d", got.Status, len(reqs))
	}
	b := reqs[0]
	reasoning, _ := b["reasoning"].(map[string]any)
	if b["store"] != false || reasoning["effort"] != "high" || b["prompt_cache_key"] != th.ID || b["parallel_tool_calls"] != true || b["tool_choice"] != nil {
		t.Fatalf("body %v", b)
	}
	usage := find(r.events(run.ID), "CUSTOM:work.usage")
	v := usage[len(usage)-1]["value"].(map[string]any)
	if v["inputTokens"] != float64(1200) || v["cachedInputTokens"] != float64(1000) || v["reasoningTokens"] != float64(30) || v["outputTokens"] != float64(50) {
		t.Fatalf("usage %v", v)
	}
}
