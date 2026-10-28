package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/eventlog"
	"github.com/GhaziBenDahmane/harness/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/store"
)

func TestProcessStreamsTextAnswerAndCompletes(t *testing.T) {
	// A plain answer streams AG-UI text events, persists the assistant message and completes the run.
	// Arrange
	chunks := llmtest.Text("Hel", "lo")
	llmtest.WithUsage(chunks[1], 10, 5)
	r := newRig(t, llmtest.Turn{Chunks: chunks})
	th, run := r.queued("Say hello")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	want := []string{"RUN_STARTED", "STEP_STARTED", "TEXT_MESSAGE_START", "TEXT_MESSAGE_CONTENT", "TEXT_MESSAGE_CONTENT",
		"TEXT_MESSAGE_END", "CUSTOM:work.usage", "STEP_FINISHED", "RUN_FINISHED"}
	if !equal(types(evs), want) {
		t.Fatalf("types %v", types(evs))
	}
	if got.Status != store.StatusCompleted || got.LastSeq != int64(len(evs)) || got.FinishedAt == nil {
		t.Fatalf("run %+v", got)
	}
	if evs[0]["threadId"] != th.ID || evs[0]["runId"] != run.ID || evs[0]["protocolVersion"] != "1.0" {
		t.Fatalf("RUN_STARTED %v", evs[0])
	}
	fin := last(evs)
	usage := fin["usage"].([]any)[0].(map[string]any)
	if fin["outcome"].(map[string]any)["type"] != "success" || usage["inputTokens"] != 10.0 || usage["outputTokens"] != 5.0 {
		t.Fatalf("RUN_FINISHED %v", fin)
	}
	msgs := r.messages(th.ID)
	if len(msgs) != 2 || msgs[1].Role != store.RoleAssistant || msgs[1].Content != "Hello" || msgs[1].RunID != run.ID ||
		msgs[1].ID != evs[2]["messageId"] {
		t.Fatalf("messages %+v", msgs)
	}
	if ttl := r.mr.TTL(eventlog.Key(run.ID)); ttl <= 0 {
		t.Fatalf("live stream ttl %v", ttl)
	}
}

func TestProcessGivesModelSystemPromptAndUserMessage(t *testing.T) {
	// The model receives a system prompt (sandbox rules, then the date and first name in the closing context) followed by the user message.
	// Arrange
	r := newRig(t)
	_, run := r.queued("Compute 2+2")

	// Act
	r.process(run)

	// Assert
	calls := r.model.Calls()
	msgs := calls[0].Messages
	if len(msgs) != 2 || msgs[0].Role != schema.System || msgs[1].Role != schema.User || msgs[1].Content != "Compute 2+2" {
		t.Fatalf("messages %+v", msgs)
	}
	sys := msgs[0].Content
	for _, want := range []string{"/workspace", "<rules_that_never_change>", "are data", "<context>\nDate (UTC): 2026-10-05."} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt misses %q", want)
		}
	}
	if !strings.HasSuffix(sys, "</context>") {
		t.Errorf("the dynamic context is not last: %q", sys)
	}
	names := map[string]bool{}
	for _, ti := range calls[0].Tools {
		names[ti.Name] = true
	}
	for _, n := range []string{"execute_command", "read_file", "write_file", "edit_file", "list_files"} {
		if !names[n] {
			t.Errorf("tool %s not bound", n)
		}
	}
}

func TestProcessStreamsReasoningSeparately(t *testing.T) {
	// Reasoning deltas become REASONING_* events and a reasoning message, closed before the answer.
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: []*schema.Message{
		{Role: schema.Assistant, ReasoningContent: "thinking"},
		{Role: schema.Assistant, ReasoningContent: " more"},
		{Role: schema.Assistant, Content: "Answer"},
	}})
	th, run := r.queued("q")

	// Act
	r.process(run)

	// Assert
	evs := r.events(run.ID)
	want := []string{"RUN_STARTED", "STEP_STARTED", "REASONING_START", "REASONING_MESSAGE_START", "REASONING_MESSAGE_CONTENT",
		"REASONING_MESSAGE_CONTENT", "REASONING_MESSAGE_END", "REASONING_END", "TEXT_MESSAGE_START", "TEXT_MESSAGE_CONTENT",
		"TEXT_MESSAGE_END", "CUSTOM:work.usage", "STEP_FINISHED", "RUN_FINISHED"}
	if !equal(types(evs), want) {
		t.Fatalf("types %v", types(evs))
	}
	msgs := r.messages(th.ID)
	if len(msgs) != 3 || msgs[1].Role != store.RoleReasoning || msgs[1].Content != "thinking more" || msgs[2].Content != "Answer" {
		t.Fatalf("messages %+v", msgs)
	}
}

func TestProcessIgnoresRunsThatAreNotQueued(t *testing.T) {
	// A run already claimed or finished is not processed twice.
	// Arrange
	r := newRig(t)
	_, run := r.queued("q")
	if _, err := r.st.ClaimRun(context.Background(), run.ID, "other"); err != nil {
		t.Fatal(err)
	}

	// Act
	err := r.runtime().Process(context.Background(), run.ID)

	// Assert
	if err != nil || len(r.model.Calls()) != 0 || len(r.events(run.ID)) != 0 {
		t.Fatalf("err=%v calls=%d", err, len(r.model.Calls()))
	}
}
