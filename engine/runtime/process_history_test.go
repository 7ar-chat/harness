package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/store"
)

func TestProcessRebuildsProviderValidHistory(t *testing.T) {
	// Earlier turns are replayed to the model without reasoning, with a synthetic result for an unanswered tool call.
	// Arrange
	r := newRig(t)
	th, first := r.queued("first question")
	ctx := context.Background()
	_ = r.st.AppendMessages(ctx,
		&store.Message{ThreadID: th.ID, RunID: first.ID, Role: store.RoleReasoning, Content: "secret thoughts"},
		&store.Message{ThreadID: th.ID, RunID: first.ID, Role: store.RoleAssistant, ToolCalls: []store.ToolCall{
			{ID: "c1", Type: "function", Function: store.ToolCallFunction{Name: "list_files", Arguments: "{}"}},
			{ID: "c2", Type: "function", Function: store.ToolCallFunction{Name: "read_file", Arguments: `{"path":"a"}`}},
		}},
		&store.Message{ThreadID: th.ID, RunID: first.ID, Role: store.RoleTool, ToolCallID: "c1", Content: "a.txt"},
		&store.Message{ThreadID: th.ID, RunID: first.ID, Role: store.RoleTool, ToolCallID: "zz", Content: "orphan"},
	)
	_, _ = r.st.ClaimRun(ctx, first.ID, "dead")
	_ = r.st.FinishRun(ctx, first.ID, "dead", store.StatusInterrupted, "", "")
	second := r.queuedOn(th, "second question")

	// Act
	r.process(second)

	// Assert
	msgs := r.model.Calls()[0].Messages[1:]
	var got []string
	for _, m := range msgs {
		got = append(got, string(m.Role)+":"+m.ToolCallID+":"+m.Content)
	}
	want := []string{"user::first question", "assistant::", "tool:c1:a.txt", "tool:c2:The tool call was interrupted before it returned a result.", "user::second question"}
	if !equal(got, want) {
		t.Fatalf("history\n got %q\nwant %q", got, want)
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, "secret") || m.ReasoningContent != "" {
			t.Fatal("reasoning leaked into the history")
		}
	}
	if msgs[1].Role != ai.RoleAssistant || len(msgs[1].ToolCalls) != 2 {
		t.Fatalf("assistant tool calls lost: %+v", msgs[1])
	}
}

func TestProcessTruncatesOldToolOutputs(t *testing.T) {
	// Tool outputs older than the last two assistant turns are cut to 2000 characters; recent ones are kept.
	// Arrange
	r := newRig(t)
	th, first := r.queued("q1")
	ctx := context.Background()
	long := strings.Repeat("x", 5000)
	call := func(id string) *store.Message {
		return &store.Message{ThreadID: th.ID, RunID: first.ID, Role: store.RoleAssistant,
			ToolCalls: []store.ToolCall{{ID: id, Type: "function", Function: store.ToolCallFunction{Name: "list_files", Arguments: "{}"}}}}
	}
	_ = r.st.AppendMessages(ctx,
		call("old"), &store.Message{ThreadID: th.ID, RunID: first.ID, Role: store.RoleTool, ToolCallID: "old", Content: long},
		call("mid"), &store.Message{ThreadID: th.ID, RunID: first.ID, Role: store.RoleTool, ToolCallID: "mid", Content: long},
		&store.Message{ThreadID: th.ID, RunID: first.ID, Role: store.RoleAssistant, Content: "answer"},
	)
	_, _ = r.st.ClaimRun(ctx, first.ID, "rt")
	_ = r.st.FinishRun(ctx, first.ID, "rt", store.StatusCompleted, "", "")
	second := r.queuedOn(th, "q2")

	// Act
	r.process(second)

	// Assert
	byID := map[string]int{}
	for _, m := range r.model.Calls()[0].Messages {
		if m.Role == ai.RoleTool {
			byID[m.ToolCallID] = len(m.Content)
		}
	}
	if byID["old"] >= 2200 || byID["old"] < 2000 || byID["mid"] != 5000 {
		t.Fatalf("tool output sizes %v", byID)
	}
}
