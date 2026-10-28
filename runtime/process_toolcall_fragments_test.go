package runtime_test

import (
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/store"
)

func frag(id, name, args string) *schema.Message {
	return &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: id, Type: "function", Function: schema.FunctionCall{Name: name, Arguments: args}}}}
}

func TestProcessMergesToolCallFragmentsWithoutIndex(t *testing.T) {
	// Providers that omit ToolCall.Index still yield one tool call per id, with all argument fragments, after leading text.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: []*schema.Message{
			{Role: schema.Assistant, Content: "Let me look."},
			frag("c1", "list_files", ""), frag("", "", `{"pa`), frag("", "", `th":"/workspace"}`),
			frag("c2", "write_file", `{"path":"a.txt",`), frag("", "", `"content":"hi"}`),
		}},
		llmtest.Turn{Chunks: llmtest.Text("done")},
	)
	th, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	starts := find(evs, "TOOL_CALL_START")
	results := find(evs, "TOOL_CALL_RESULT")
	if got.Status != store.StatusCompleted || len(starts) != 2 || starts[0]["toolCallName"] != "list_files" || starts[1]["toolCallName"] != "write_file" ||
		len(find(evs, "TOOL_CALL_END")) != 2 || len(results) != 2 {
		t.Fatalf("status %s types %v", got.Status, types(evs))
	}
	for _, res := range results {
		if res["metadata"] != nil {
			t.Fatalf("tool failed: %v", res)
		}
	}
	msgs := r.messages(th.ID)
	calls := msgs[1].ToolCalls
	if msgs[1].Content != "Let me look." || len(calls) != 2 || calls[0].Function.Arguments != `{"path":"/workspace"}` ||
		calls[1].ID != "c2" || calls[1].Function.Arguments != `{"path":"a.txt","content":"hi"}` {
		t.Fatalf("assistant message %+v", msgs[1])
	}
}

func TestProcessKeepsToolsOnLastStepButForbidsCalls(t *testing.T) {
	// The last step keeps tool definitions (history has tool messages) and sends tool_choice=none.
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "c1", "list_files", `{}`)}}, llmtest.Turn{Chunks: llmtest.Text("summary")})
	r.agent.MaxSteps = 2
	_, run := r.queued("q")

	// Act
	r.process(run)

	// Assert
	calls := r.model.Calls()
	lastCall := calls[len(calls)-1]
	if len(lastCall.Tools) == 0 || lastCall.ToolChoice == nil || *lastCall.ToolChoice != schema.ToolChoiceForbidden || calls[0].ToolChoice != nil {
		t.Fatalf("last call tools=%d choice=%v first choice=%v", len(lastCall.Tools), lastCall.ToolChoice, calls[0].ToolChoice)
	}
}

func TestProcessKeepsSeveralIDlessCallsOfOneChunkApart(t *testing.T) {
	// A chunk carrying several tool calls without ID or Index yields one call each, with valid JSON arguments.
	// Arrange
	two := &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
		{Function: schema.FunctionCall{Name: "list_files", Arguments: `{"path":"/workspace"}`}},
		{Function: schema.FunctionCall{Name: "write_file", Arguments: `{"path":"a.txt","content":"x"}`}},
	}}
	r := newRig(t, llmtest.Turn{Chunks: []*schema.Message{two}}, llmtest.Turn{Chunks: llmtest.Text("ok")})
	th, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	calls := r.messages(th.ID)[1].ToolCalls
	if got.Status != store.StatusCompleted || len(calls) != 2 || calls[0].Function.Arguments != `{"path":"/workspace"}` ||
		calls[1].Function.Name != "write_file" || calls[0].ID == "" || calls[0].ID == calls[1].ID {
		t.Fatalf("calls %+v", calls)
	}
	for _, res := range find(r.events(run.ID), "TOOL_CALL_RESULT") {
		if res["metadata"] != nil {
			t.Fatalf("tool failed: %v", res)
		}
	}
}
