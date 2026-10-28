package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/store"
)

// failing is a tool that always fails with the message of its "msg" argument.
func failing() *fakeTool {
	return &fakeTool{name: "convert", run: func(_ context.Context, args string) (string, error) {
		return "", errors.New("conversion failed: " + args)
	}}
}

func callTurn(id, name, args string) llmtest.Turn {
	return llmtest.Turn{Chunks: streamedCall(id, name, args)}
}

func TestTheSameErrorThreeTimesGetsANoteWithTheError(t *testing.T) {
	// The third occurrence of one error in a task (calls with different arguments) gets a note quoting the error and asking for another approach.
	// Arrange
	same := &fakeTool{name: "convert", run: func(context.Context, string) (string, error) {
		return "", errors.New("soffice: source file could not be loaded")
	}}
	r := newRig(t, callTurn("c1", "convert", `{"f":1}`), callTurn("c2", "convert", `{"f":2}`), callTurn("c3", "convert", `{"f":3}`),
		llmtest.Turn{Chunks: llmtest.Text("I could not convert it.")})
	r.agent.MaxSteps = 10
	r.withTools(same)
	_, run := r.queued("convert")

	// Act
	r.process(run)

	// Assert
	results := find(r.events(run.ID), "TOOL_CALL_RESULT")
	if strings.Contains(results[1]["content"].(string), "3rd time") || !strings.Contains(results[2]["content"].(string),
		"This is the 3rd time this error occurred in this task: soffice: source file could not be loaded") {
		t.Fatalf("results %v / %v", results[1]["content"], results[2]["content"])
	}
}

func TestAlternatingBetweenTwoCallsIsStopped(t *testing.T) {
	// A-B-A-B-A-B over six calls: the sixth is not executed and the model is told to change approach.
	// Arrange
	ran := 0
	a := &fakeTool{name: "a", run: func(context.Context, string) (string, error) { ran++; return "ok", nil }}
	b := &fakeTool{name: "b", run: func(context.Context, string) (string, error) { ran++; return "ok", nil }}
	var turns []llmtest.Turn
	for i := 0; i < 6; i++ {
		name := []string{"a", "b"}[i%2]
		turns = append(turns, callTurn(fmt.Sprint("c", i), name, `{}`))
	}
	turns = append(turns, llmtest.Turn{Chunks: llmtest.Text("Stopping.")})
	r := newRig(t, turns...)
	r.agent.MaxSteps = 10
	r.withTools(a, b)
	_, run := r.queued("go")

	// Act
	r.process(run)

	// Assert
	results := find(r.events(run.ID), "TOOL_CALL_RESULT")
	if ran != 5 || !strings.Contains(results[5]["content"].(string), "alternate between the same two calls (a and b)") {
		t.Fatalf("ran %d last %v", ran, results[5]["content"])
	}
}

func TestEightFailedCallsInARowForceAnAnswerWithoutTools(t *testing.T) {
	// The fifth failed call in a row gets a note suggesting ask_user; after the eighth the next call forbids tools and asks for a final answer, so a stuck run ends early.
	// Arrange
	var turns []llmtest.Turn
	for i := 0; i < 8; i++ {
		turns = append(turns, callTurn(fmt.Sprint("c", i), "convert", fmt.Sprintf(`{"try":%d}`, i)))
	}
	turns = append(turns, llmtest.Turn{Chunks: llmtest.Text("I am blocked: the converter keeps failing.")})
	r := newRig(t, turns...)
	r.agent.MaxSteps = 30
	r.withTools(failing())
	_, run := r.queued("convert it")

	// Act
	got := r.process(run)

	// Assert
	calls := r.model.Calls()
	results := find(r.events(run.ID), "TOOL_CALL_RESULT")
	if got.Status != store.StatusCompleted || len(calls) != 9 {
		t.Fatalf("status %s calls %d", got.Status, len(calls))
	}
	if !strings.Contains(results[4]["content"].(string), "5 tool calls in a row have failed") || !strings.Contains(results[4]["content"].(string), "ask_user") {
		t.Fatalf("fifth result %v", results[4]["content"])
	}
	final := calls[8]
	if final.ToolChoice == nil || *final.ToolChoice != schema.ToolChoiceForbidden ||
		final.Messages[len(final.Messages)-1].Content != fmt.Sprintf(agent.TooManyErrorsNote, 8) {
		t.Fatalf("final call choice %v last %+v", final.ToolChoice, final.Messages[len(final.Messages)-1])
	}
}

func TestLoopGuardStateSurvivesApprovalResumes(t *testing.T) {
	// In interactive mode every approved command runs in a new resumed run; the third identical command of the turn is still caught as a loop and not run (A-21).
	// Arrange
	cmd := `{"command":"python3 broken.py"}`
	r := newApprovalRig(t, callTurn("c1", "execute_command", cmd), callTurn("c2", "execute_command", cmd),
		callTurn("c3", "execute_command", cmd), llmtest.Turn{Chunks: llmtest.Text("blocked")})
	r.agent.MaxSteps = 10
	th, run := r.interactive("run the script")

	// Act
	r.process(run)
	second := r.process(r.decide(th, store.DecisionApprove, store.ApprovalResponse{}))
	third := r.process(r.decide(th, store.DecisionApprove, store.ApprovalResponse{}))

	// Assert
	execs := 0
	for _, sb := range r.backend.Created() {
		for _, e := range sb.Execs() {
			if e.Command == "python3 broken.py" {
				execs++
			}
		}
	}
	results := find(r.events(third.ID), "TOOL_CALL_RESULT")
	if second.Status != store.StatusCompleted || execs != 2 || len(results) == 0 ||
		!strings.Contains(results[len(results)-1]["content"].(string), "identical arguments 3 times in a row") {
		t.Fatalf("execs %d results %v", execs, results)
	}
}

func TestUnfinishedPlanIsRemindedInAResumedRun(t *testing.T) {
	// A plan written before an ask_user still blocks the resumed run's stop while items are open (A-21).
	// Arrange
	plan := `{"items":[{"id":"1","title":"Load data","status":"completed"},{"id":"2","title":"Write the report","status":"in_progress"}]}`
	r := newRig(t,
		callTurn("p1", "update_plan", plan),
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "ask", "ask_user", askArgs)}},
		llmtest.Turn{Chunks: llmtest.Text("Done.")},
		llmtest.Turn{Chunks: llmtest.Text("Now done.")},
	)
	r.agent.MaxSteps = 10
	th, first := r.queued("report")
	r.process(first)
	q := r.thread(th.ID).PendingQuestion
	second := r.create(th, store.NewRun{Resume: &store.Resume{InterruptID: q.InterruptID, Content: "PDF"}})

	// Act
	r.process(second)

	// Assert
	calls := r.model.Calls()
	last := calls[len(calls)-1].Messages
	if n := len(last); !strings.Contains(last[n-1].Content, "Your plan still has 1 unfinished item(s)") {
		t.Fatalf("no plan reminder: %+v", last[n-1])
	}
}

func TestEveryEighthStepRemindsTheBudgetAndThePlan(t *testing.T) {
	// Every 8 steps the model gets a system-reminder with the steps left and the open plan items.
	// Arrange
	plan := `{"items":[{"id":"1","title":"Collect","status":"in_progress"},{"id":"2","title":"Write","status":"pending"}]}`
	turns := []llmtest.Turn{callTurn("p", "update_plan", plan)}
	ok := &fakeTool{name: "step"}
	for i := 0; i < 8; i++ {
		turns = append(turns, callTurn(fmt.Sprint("s", i), "step", fmt.Sprintf(`{"n":%d}`, i)))
	}
	turns = append(turns, llmtest.Turn{Chunks: llmtest.Text("done")})
	r := newRig(t, turns...)
	r.agent.MaxSteps = 20
	r.withTools(ok)
	_, run := r.queued("go")

	// Act
	r.process(run)

	// Assert
	ninth := r.model.Calls()[8].Messages
	want := "<system-reminder>\nStep 9 of 20 (12 left).\nPlan: 2 of 2 items open: \"Collect\" (in_progress), \"Write\" (pending).\n</system-reminder>"
	if got := ninth[len(ninth)-1].Content; got != want {
		t.Fatalf("reminder %q", got)
	}
	for i, c := range r.model.Calls()[:8] {
		if m := c.Messages[len(c.Messages)-1]; strings.Contains(m.Content, "Step ") {
			t.Fatalf("reminder at call %d", i+1)
		}
	}
}
