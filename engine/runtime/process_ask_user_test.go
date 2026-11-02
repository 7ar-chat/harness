package runtime_test

import (
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/store"
)

const askArgs = `{"question":"Which format do you want?","options":[{"label":"PDF","description":"For printing"},{"label":"Word"}],"allowFreeText":false}`

func TestProcessAskUserEndsTheRunWithAnInterrupt(t *testing.T) {
	// ask_user ends the run (status completed) with RUN_FINISHED{outcome: interrupt} after closing the step, and opens the thread's pending question.
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: append(llmtest.Text("Let me check one thing."), llmtest.ToolCall(0, "call_ask", "ask_user", askArgs))})
	th, run := r.queued("make the report")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	want := []string{"RUN_STARTED", "STEP_STARTED", "TEXT_MESSAGE_START", "TEXT_MESSAGE_CONTENT", "TEXT_MESSAGE_END",
		"TOOL_CALL_START", "TOOL_CALL_ARGS", "TOOL_CALL_END", "CUSTOM:work.usage", "STEP_FINISHED", "RUN_FINISHED"}
	if got.Status != store.StatusCompleted || !equal(types(evs), want) || len(r.model.Calls()) != 1 {
		t.Fatalf("status %s types %v calls %d", got.Status, types(evs), len(r.model.Calls()))
	}
	outcome := last(evs)["outcome"].(map[string]any)
	intr := outcome["interrupts"].([]any)[0].(map[string]any)
	schemaV := intr["responseSchema"].(map[string]any)
	if outcome["type"] != "interrupt" || intr["reason"] != "ask_user" || intr["message"] != "Which format do you want?" ||
		intr["toolCallId"] != "call_ask" || intr["id"] == "" || schemaV["allowFreeText"] != false || len(schemaV["options"].([]any)) != 2 {
		t.Fatalf("outcome %v", outcome)
	}
	q := r.thread(th.ID).PendingQuestion
	if q == nil || q.InterruptID != intr["id"] || q.RunID != run.ID || q.ToolCallID != "call_ask" || q.AllowFreeText ||
		len(q.Options) != 2 || q.Options[0].Description != "For printing" {
		t.Fatalf("pending question %+v", q)
	}
}

func TestProcessResumedRunGivesTheAnswerAsToolResult(t *testing.T) {
	// The run created with resume replays the answer as TOOL_CALL_RESULT first, and its model sees ask_user followed by the answer.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "call_ask", "ask_user", askArgs)}},
		llmtest.Turn{Chunks: llmtest.Text("Here is the PDF")},
	)
	th, first := r.queued("make the report")
	r.process(first)
	q := r.thread(th.ID).PendingQuestion
	second := r.create(th, store.NewRun{Resume: &store.Resume{InterruptID: q.InterruptID, Content: "The user chose: PDF"}})

	// Act
	got := r.process(second)

	// Assert
	evs := r.events(second.ID)
	if got.Status != store.StatusCompleted || types(evs)[1] != "TOOL_CALL_RESULT" || types(evs)[2] != "STEP_STARTED" ||
		evs[1]["toolCallId"] != "call_ask" || evs[1]["content"] != "The user chose: PDF" {
		t.Fatalf("status %s types %v", got.Status, types(evs))
	}
	msgs := r.model.Calls()[1].Messages
	n := len(msgs)
	if msgs[n-2].Role != ai.RoleAssistant || msgs[n-2].ToolCalls[0].ID != "call_ask" ||
		msgs[n-1].Role != ai.RoleTool || msgs[n-1].ToolCallID != "call_ask" || msgs[n-1].Content != "The user chose: PDF" {
		t.Fatalf("history %+v", msgs[1:])
	}
	if r.thread(th.ID).PendingQuestion != nil {
		t.Fatal("question still pending")
	}
}

func TestProcessAnswerAfterLeftoverSteerStaysNextToItsCall(t *testing.T) {
	// A steer kept in the thread after the question does not separate the ask_user call from its later answer in the model history.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "call_ask", "ask_user", `{"question":"Which year?"}`)}},
		llmtest.Turn{Chunks: llmtest.Text("2025 it is")},
	)
	th, first := r.queued("report")
	r.process(first)
	_ = r.st.AppendMessages(t.Context(), &store.Message{ThreadID: th.ID, RunID: first.ID, Role: store.RoleUser, Content: "also add a chart",
		Metadata: map[string]any{"steer": true, "delivered": false}})
	q := r.thread(th.ID).PendingQuestion
	second := r.create(th, store.NewRun{Resume: &store.Resume{InterruptID: q.InterruptID, Content: "The user answered: 2025"}})

	// Act
	r.process(second)

	// Assert
	var got []string
	for _, m := range r.model.Calls()[1].Messages[1:] {
		got = append(got, string(m.Role)+":"+m.Content)
	}
	want := []string{"user:report", "assistant:", "tool:The user answered: 2025", "user:[The user added while you were working]: also add a chart"}
	if !equal(got, want) {
		t.Fatalf("history\n got %q\nwant %q", got, want)
	}
}

func TestProcessSecondQuestionInOneStepIsRefused(t *testing.T) {
	// Only the first ask_user of a step interrupts; a second one gets an error result.
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: []*ai.Message{
		llmtest.ToolCall(0, "q1", "ask_user", `{"question":"A?"}`), llmtest.ToolCall(1, "q2", "ask_user", `{"question":"B?"}`)}})
	th, run := r.queued("q")

	// Act
	r.process(run)

	// Assert
	results := find(r.events(run.ID), "TOOL_CALL_RESULT")
	if len(results) != 1 || results[0]["toolCallId"] != "q2" || !strings.Contains(results[0]["content"].(string), "one question") {
		t.Fatalf("results %v", results)
	}
	if q := r.thread(th.ID).PendingQuestion; q == nil || q.ToolCallID != "q1" || q.Question != "A?" || !q.AllowFreeText {
		t.Fatalf("pending %+v", q)
	}
}
