package runtime_test

import (
	"context"
	"github.com/7ar-chat/harness/ai/openai"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
)

// assertBalanced checks that every START has its END before the terminal event (strict AG-UI order).
func assertBalanced(t *testing.T, evs []event) {
	t.Helper()
	open := map[string]int{}
	pairs := map[string]string{
		"STEP_STARTED": "STEP_FINISHED", "TEXT_MESSAGE_START": "TEXT_MESSAGE_END", "REASONING_START": "REASONING_END",
		"REASONING_MESSAGE_START": "REASONING_MESSAGE_END", "TOOL_CALL_START": "TOOL_CALL_END",
	}
	for i, e := range evs {
		typ := e["type"].(string)
		if end, ok := pairs[typ]; ok {
			open[end]++
		}
		for _, end := range pairs {
			if typ == end {
				open[end]--
			}
		}
		if (typ == "RUN_FINISHED" || typ == "RUN_ERROR") && i != len(evs)-1 {
			t.Fatalf("terminal event not last: %v", types(evs))
		}
	}
	for end, n := range open {
		if n != 0 {
			t.Fatalf("%d unclosed (%s) in %v", n, end, types(evs))
		}
	}
}

func TestCancelledRunClosesOpenSteps(t *testing.T) {
	// A run cancelled during a tool call closes its step before RUN_FINISHED.
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "c1", "execute_command", `{"command":"sleep 9"}`)}})
	started := make(chan struct{})
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		sb.SetExec(func(ctx context.Context, _ sandbox.ExecRequest) (sandbox.ExecResult, error) {
			close(started)
			<-ctx.Done()
			return sandbox.ExecResult{}, ctx.Err()
		})
	}
	_, run := r.queued("q")
	done := make(chan struct{})
	go func() { r.process(run); close(done) }()
	<-started

	// Act
	_ = r.q.PublishCancel(context.Background(), run.ID)

	// Assert
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("not cancelled")
	}
	assertBalanced(t, r.events(run.ID))
}

func TestFailedRunsCloseOpenMessagesAndHideRawErrors(t *testing.T) {
	// A model failure closes open text and step events, and RUN_ERROR carries a generic message, not the raw gateway error.
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: []*ai.Message{{Role: ai.RoleAssistant, ReasoningContent: "hmm"}, {Role: ai.RoleAssistant, Content: "par"}},
		FailAfter: &openai.APIError{Status: 500, Message: "upstream http://llm-gateway.internal:5000 refused"}})
	r.agent.StepRetries = -1 // the failure is final (retries are covered by the resilience tests)
	_, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	assertBalanced(t, evs)
	msg := last(evs)["message"].(string)
	if last(evs)["code"] != "model_error" || strings.Contains(msg, "llm-gateway") || strings.Contains(got.Error, "llm-gateway") || msg == "" {
		t.Fatalf("message %q run error %q", msg, got.Error)
	}
	if got.Status != store.StatusFailed {
		t.Fatalf("status %s", got.Status)
	}
}
