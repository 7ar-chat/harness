//go:build unix

package longtest_test

import (
	"context"
	"github.com/7ar-chat/harness/ai/openai"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/longtest"
)

func customEvents(o *longtest.Outcome, name string) int {
	n := 0
	for _, ev := range o.Events {
		if strings.Contains(string(ev.Data), `"name":"`+name+`"`) {
			n++
		}
	}
	return n
}

func anyRequestContains(o *longtest.Outcome, sub string) bool {
	for _, c := range o.Calls {
		if strings.Contains(longtest.CallsText(c), sub) {
			return true
		}
	}
	return false
}

func TestModelStreamErrorsMidRunAreRetriedAndLeaveNoTrace(t *testing.T) {
	// A stream cut after partial text and call, and a 503 before any chunk, each cost one retry; the run goes on to step 100 and the failed attempt never reaches the history.
	// Arrange
	failed := map[int]bool{}
	wrap := func(step int, turn llmtest.Turn) llmtest.Turn {
		if failed[step] { // the retry plays the step again
			return turn
		}
		switch step {
		case 30:
			failed[step] = true
			return llmtest.Turn{Chunks: []*ai.Message{{Role: ai.RoleAssistant, Content: "PARTIAL-ATTEMPT-TEXT"}, llmtest.ToolCall(0, "cx", "read_file", `{"path":"/work`)},
				FailAfter: &openai.APIError{Status: 502, Message: "bad gateway"}}
		case 70:
			failed[step] = true
			return llmtest.Turn{Err: &openai.APIError{Status: 503, Message: "overloaded"}}
		}
		return turn
	}

	// Act
	o := longtest.Run(t, longtest.Scenario{Steps: 100, Window: 60000, Wrap: wrap})

	// Assert
	if o.Status != "finished" {
		t.Fatalf("status %q after %d steps", o.Status, o.Steps)
	}
	if o.Steps != 100 || len(o.Calls) != 102 {
		t.Fatalf("steps %d, model calls %d (want 100 steps and 2 retried calls)", o.Steps, len(o.Calls))
	}
	if n := customEvents(o, "work.retry"); n != 2 {
		t.Errorf("%d work.retry events, want 2", n)
	}
	if anyRequestContains(o, "PARTIAL-ATTEMPT-TEXT") {
		t.Error("the text of the failed attempt was sent back to the model")
	}
}

func TestHangingCommandTimesOutIntoABackgroundJobAndTheRunContinues(t *testing.T) {
	// execute_command with a 1 s timeout on a command that sleeps becomes a background job; the model is told so and the run finishes quickly instead of waiting for it.
	// Arrange
	action := func(step int) (string, string) {
		if step == 20 {
			return "execute_command", `{"command":"sleep 3","timeoutSeconds":1}`
		}
		return longtest.DefaultAction(step)
	}

	// Act
	o := longtest.Run(t, longtest.Scenario{Steps: 40, Window: 60000, Action: action})

	// Assert
	if o.Status != "finished" {
		t.Fatalf("status %q", o.Status)
	}
	res := o.Calls[20].Messages[len(o.Calls[20].Messages)-1] // the request after step 20
	if res.Role != ai.RoleTool || !strings.Contains(res.Content, "job") {
		t.Fatalf("result of the hanging command: %.300q", res.Content)
	}
	// (The job's process and its watcher goroutine outlive the run until the command ends: by design.)
	if o.Wall > 6*time.Second {
		t.Errorf("the run waited for the sleeping job (%v)", o.Wall)
	}
}

func TestCancelCutsAToolThatNeverReturnsAndLeavesNoGoroutine(t *testing.T) {
	// A custom tool blocking forever is released by Cancel: the run ends cancelled, every goroutine winds down.
	// Arrange
	var entered atomic.Bool
	hang := longtest.ExtraTool("hang", func(ctx context.Context, _ string) (string, error) {
		entered.Store(true)
		<-ctx.Done()
		return "", ctx.Err()
	})
	action := func(step int) (string, string) {
		if step == 10 {
			return "hang", `{}`
		}
		return longtest.DefaultAction(step)
	}

	// Act
	o := longtest.Run(t, longtest.Scenario{Steps: 40, Window: 60000, Tools: []tools.Provider{hang}, Action: action,
		CancelAtStep: 10, CancelDelay: 300 * time.Millisecond, Timeout: 30 * time.Second})

	// Assert
	if !entered.Load() || o.Status != "cancelled" {
		t.Fatalf("tool entered %v, status %q", entered.Load(), o.Status)
	}
	if len(o.Calls) != 10 {
		t.Errorf("%d model calls: the run went on after the cancel", len(o.Calls))
	}
	if o.GoroutinesAfter > o.GoroutinesBefore {
		t.Errorf("goroutines %d -> %d", o.GoroutinesBefore, o.GoroutinesAfter)
	}
}

func TestAnswerCutByTheOutputLimitIsContinued(t *testing.T) {
	// A text answer ending with finish_reason=length gets one continuation note and the run goes on.
	// Arrange
	wrap := func(step int, turn llmtest.Turn) llmtest.Turn {
		if step == 40 {
			return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(&ai.Message{Role: ai.RoleAssistant, Content: "I will now continue with the modules and then"}, "length")}}
		}
		return turn
	}

	// Act
	o := longtest.Run(t, longtest.Scenario{Steps: 60, Window: 60000, Wrap: wrap})

	// Assert
	if o.Status != "finished" || o.Steps != 60 {
		t.Fatalf("status %q steps %d", o.Status, o.Steps)
	}
	found := false
	for _, m := range o.Calls[40].Messages {
		found = found || m.Content == agent.ContinueAfterLengthNote
	}
	if !found {
		t.Error("the request after the cut has no continuation note")
	}
}

func TestToolCallCutByTheOutputLimitDoesNotEndTheRun(t *testing.T) {
	// A tool call whose JSON arguments were cut by the output limit is an error result the model can recover from.
	// Arrange
	wrap := func(step int, turn llmtest.Turn) llmtest.Turn {
		if step == 40 {
			return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "cut", "edit_file", `{"path":"/workspace/pkg/m01/f01.go","oldText":"// TODO(m01`), "length")}}
		}
		return turn
	}

	// Act
	o := longtest.Run(t, longtest.Scenario{Steps: 60, Window: 60000, Wrap: wrap})

	// Assert
	if o.Status != "finished" {
		t.Fatalf("status %q", o.Status)
	}
	var res *ai.Message
	for _, m := range o.Calls[40].Messages {
		if m.Role == ai.RoleTool && m.ToolCallID == "cut" {
			res = m
		}
	}
	if res == nil {
		t.Fatal("the cut call got no result")
	}
	t.Logf("result of the cut call: %.200q", res.Content)
}

func TestStuckLoopOnIdenticalCallsIsBlockedAndEnds(t *testing.T) {
	// The model repeats the same call: the third identical call in a row is not executed; the run does not spin to the step limit.
	// Arrange
	var runs atomic.Int32
	probe := longtest.ExtraTool("probe", func(context.Context, string) (string, error) { runs.Add(1); return "same failure", nil })
	action := func(step int) (string, string) {
		if step >= 20 {
			return "probe", `{"path":"a.docx","page":1}`
		}
		return longtest.DefaultAction(step)
	}

	// Act
	o := longtest.Run(t, longtest.Scenario{Steps: 150, MaxSteps: 60, Window: 60000, Tools: []tools.Provider{probe}, Action: action, NoFinish: true})

	// Assert
	if o.Status != "finished" && o.Status != "max_steps" {
		t.Fatalf("status %q", o.Status)
	}
	if runs.Load() > 2 {
		t.Errorf("the tool ran %d times, the guard should stop it at 2", runs.Load())
	}
	if !anyRequestContains(o, "identical arguments 3 times in a row") {
		t.Error("the model was never told about the loop")
	}
	t.Logf("status %s after %d model calls (limit 60); tool ran %d times", o.Status, len(o.Calls), runs.Load())
}

func TestStepBudgetEndsWithAForcedAnswerWithoutTools(t *testing.T) {
	// A model that never stops calling tools gets the step-limit nudge with tools forbidden on the last allowed step, and the run ends gracefully.
	// Arrange / Act
	o := longtest.Run(t, longtest.Scenario{Steps: 1000, MaxSteps: 45, Window: 60000, NoFinish: true})

	// Assert
	if len(o.Calls) != 45 {
		t.Fatalf("%d model calls, want exactly the budget of 45", len(o.Calls))
	}
	last := o.Calls[44]
	if last.ToolChoice == nil || *last.ToolChoice != ai.ToolChoiceForbidden {
		t.Errorf("tool choice on the last step: %v", last.ToolChoice)
	}
	if got := last.Messages[len(last.Messages)-1].Content; got != agent.StepLimitNudge {
		t.Errorf("last message %q", got)
	}
	if o.Status != "finished" && o.Status != "max_steps" {
		t.Errorf("status %q", o.Status)
	}
	if o.Steps != 45 {
		t.Errorf("steps %d", o.Steps)
	}
	t.Logf("status %s", o.Status)
}
