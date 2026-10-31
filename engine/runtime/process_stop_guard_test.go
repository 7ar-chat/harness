package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/queue"
)

const emptyNudge = "You stopped without answering. Continue the task, or give the user a short summary of what you did and found."

func TestProcessNudgesModelOnceAfterEmptyAnswer(t *testing.T) {
	// An answer without text or tool calls gets one (unpersisted) nudge and the run continues.
	// Arrange
	r := newRig(t, llmtest.Turn{}, llmtest.Turn{Chunks: llmtest.Text("Here is what I found.")})
	th, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	calls := r.model.Calls()
	if got.Status != store.StatusCompleted || len(calls) != 2 {
		t.Fatalf("status %s calls %d", got.Status, len(calls))
	}
	second := calls[1].Messages
	if lastMsg := second[len(second)-1]; lastMsg.Role != ai.RoleUser || lastMsg.Content != emptyNudge {
		t.Fatalf("second call %+v", second)
	}
	msgs := r.messages(th.ID)
	if len(msgs) != 2 || msgs[1].Content != "Here is what I found." {
		t.Fatalf("messages %+v", msgs)
	}
}

func TestProcessFailsAfterSecondEmptyAnswer(t *testing.T) {
	// A second empty answer in a row ends the run with RUN_ERROR empty_answer (harness v3 §1) instead of nudging again.
	// Arrange
	r := newRig(t, llmtest.Turn{}, llmtest.Turn{})
	_, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	if got.Status != store.StatusFailed || len(r.model.Calls()) != 2 || last(r.events(run.ID))["code"] != "empty_answer" {
		t.Fatalf("status %s calls %d", got.Status, len(r.model.Calls()))
	}
}

func TestProcessRemindsModelBeforeItStopsWithUnfinishedWork(t *testing.T) {
	// A StopGuard reminder given when the model answers is sent as a system-reminder user message (not persisted, no new event type) and the run continues.
	// Arrange
	pending := true
	todos := &guardTool{fakeTool: fakeTool{name: "todo"}, reminder: func() string {
		if pending {
			pending = false
			return "2 todo items are not completed."
		}
		return ""
	}}
	quiet := &guardTool{fakeTool: fakeTool{name: "quiet"}, reminder: func() string { return "" }}
	r := newRig(t, llmtest.Turn{Chunks: llmtest.Text("Done.")}, llmtest.Turn{Chunks: llmtest.Text("Now everything is done.")})
	r.withTools(todos, quiet)
	th, run := r.queued("make the report")

	// Act
	got := r.process(run)

	// Assert
	calls := r.model.Calls()
	if got.Status != store.StatusCompleted || len(calls) != 2 {
		t.Fatalf("status %s calls %d", got.Status, len(calls))
	}
	second := calls[1].Messages
	if lastMsg := second[len(second)-1]; lastMsg.Role != ai.RoleUser || lastMsg.Content != "<system-reminder>\n2 todo items are not completed.\n</system-reminder>" {
		t.Fatalf("second call %+v", second)
	}
	var contents []string
	for _, m := range r.messages(th.ID) {
		contents = append(contents, m.Role+":"+m.Content)
	}
	if !equal(contents, []string{"user:make the report", "assistant:Done.", "assistant:Now everything is done."}) {
		t.Fatalf("messages %q", contents)
	}
	for _, typ := range types(r.events(run.ID)) {
		if strings.Contains(typ, "reminder") {
			t.Fatalf("reminder event %s", typ)
		}
	}
}

func TestProcessGivesAtMostTwoStopReminders(t *testing.T) {
	// A StopGuard that always objects is consulted for at most two reminders per run, then the run finishes.
	// Arrange
	nag := &guardTool{fakeTool: fakeTool{name: "nag"}, reminder: func() string { return "not finished" }}
	r := newRig(t, llmtest.Turn{Chunks: llmtest.Text("a")}, llmtest.Turn{Chunks: llmtest.Text("b")}, llmtest.Turn{Chunks: llmtest.Text("c")},
		llmtest.Turn{Chunks: llmtest.Text("unexpected")})
	r.withTools(nag)
	_, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	if got.Status != store.StatusCompleted || len(r.model.Calls()) != 3 {
		t.Fatalf("status %s calls %d", got.Status, len(r.model.Calls()))
	}
}

func TestProcessAcceptsSteersAfterAStopReminder(t *testing.T) {
	// After a reminder continues the run, a steer pushed during the next answer is accepted and delivered in a later step.
	// Arrange
	var runID string
	var pushErr error
	once := true
	guard := &guardTool{fakeTool: fakeTool{name: "todo"}, reminder: func() string {
		if once {
			once = false
			return "finish the chart"
		}
		return ""
	}}
	r := newRig(t)
	r.model = llmtest.New(
		llmtest.Turn{Chunks: llmtest.Text("Done.")},
		llmtest.Turn{Chunks: llmtest.Text("Chart done."), OnCall: func() {
			pushErr = r.q.PushSteer(context.Background(), runID, queue.Steer{ID: "s-1", Text: "use blue"})
		}},
		llmtest.Turn{Chunks: llmtest.Text("Blue it is.")},
	)
	r.agent.Models = llmtest.Factory{M: r.model}
	r.withTools(guard)
	_, run := r.queued("q")
	runID = run.ID

	// Act
	got := r.process(run)

	// Assert
	calls := r.model.Calls()
	if got.Status != store.StatusCompleted || pushErr != nil || len(calls) != 3 {
		t.Fatalf("status %s push %v calls %d", got.Status, pushErr, len(calls))
	}
	third := calls[2].Messages
	if !strings.Contains(third[len(third)-1].Content, "use blue") {
		t.Fatalf("steer not delivered: %+v", third[len(third)-1])
	}
}

func TestProcessKeepsAcceptingSteersWhileAReminderContinuesTheRun(t *testing.T) {
	// Stop guards are asked before steers are closed: the run only stops accepting steers when it really stops.
	// Arrange
	once := true
	guard := &guardTool{fakeTool: fakeTool{name: "todo"}, reminder: func() string {
		if once {
			once = false
			return "finish the chart"
		}
		return ""
	}}
	r := newRig(t, llmtest.Turn{Chunks: llmtest.Text("Done.")}, llmtest.Turn{Chunks: llmtest.Text("Chart done.")})
	r.withTools(guard)
	steers := &countingSteers{SteerSource: r.q}
	r.agent.Steers = steers
	_, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	if got.Status != store.StatusCompleted || len(r.model.Calls()) != 2 || steers.finalizes.Load() != 1 {
		t.Fatalf("status %s calls %d finalizes %d", got.Status, len(r.model.Calls()), steers.finalizes.Load())
	}
}

func TestProcessKeepsAcceptingSteersWhileANudgeContinuesTheRun(t *testing.T) {
	// The empty-answer nudge is decided before steers are closed, like a reminder.
	// Arrange
	r := newRig(t, llmtest.Turn{}, llmtest.Turn{Chunks: llmtest.Text("Found it.")})
	steers := &countingSteers{SteerSource: r.q}
	r.agent.Steers = steers
	_, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	if got.Status != store.StatusCompleted || len(r.model.Calls()) != 2 || steers.finalizes.Load() != 1 {
		t.Fatalf("status %s calls %d finalizes %d", got.Status, len(r.model.Calls()), steers.finalizes.Load())
	}
}

func planCall(id, items string) llmtest.Turn {
	return llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, id, "update_plan", `{"items":`+items+`}`)}}
}

func TestProcessRemindsAboutUnfinishedItemsOfAPlanWrittenInThisRun(t *testing.T) {
	// update_plan is a stop guard: stopping with pending or in-progress items of a plan written in this run gets a reminder naming them.
	// Arrange
	r := newRig(t,
		planCall("p1", `[{"id":"1","title":"Load the data","status":"completed"},{"id":"2","title":"Plot it","status":"in_progress"},{"id":"3","title":"Write the report","status":"pending"}]`),
		llmtest.Turn{Chunks: llmtest.Text("Loaded.")},
		planCall("p2", `[{"id":"1","title":"Load the data","status":"completed"},{"id":"2","title":"Plot it","status":"completed"},{"id":"3","title":"Write the report","status":"completed"}]`),
		llmtest.Turn{Chunks: llmtest.Text("All done.")},
		llmtest.Turn{Chunks: llmtest.Text("unexpected")},
	)
	_, run := r.queued("analyse sales")

	// Act
	got := r.process(run)

	// Assert
	calls := r.model.Calls()
	if got.Status != store.StatusCompleted || len(calls) != 4 {
		t.Fatalf("status %s calls %d", got.Status, len(calls))
	}
	reminder := calls[2].Messages[len(calls[2].Messages)-1].Content
	if !strings.HasPrefix(reminder, "<system-reminder>\n") || !strings.Contains(reminder, "Plot it") || !strings.Contains(reminder, "Write the report") ||
		strings.Contains(reminder, "Load the data") || !strings.Contains(reminder, "update_plan") {
		t.Fatalf("reminder %q", reminder)
	}
}

func TestProcessDoesNotRemindAboutAFinishedPlanOrAPlanOfAnEarlierRun(t *testing.T) {
	// No reminder when every item of this run's plan is completed, nor for unfinished items of a plan written by an earlier run.
	// Arrange
	r := newRig(t,
		planCall("p1", `[{"id":"1","title":"Plot it","status":"pending"}]`),
		llmtest.Turn{Chunks: llmtest.Text("Paused.")},
		llmtest.Turn{Chunks: llmtest.Text("Sure, here is a joke.")},
	)
	th, first := r.queued("plot sales")
	r.agent.MaxSteps = 2 // the first run's answer comes on its last step: no reminder
	r.process(first)
	r.agent.MaxSteps = 5
	second := r.queuedOn(th, "tell me a joke")
	finished := newRig(t,
		planCall("p1", `[{"id":"1","title":"Plot it","status":"completed"}]`),
		llmtest.Turn{Chunks: llmtest.Text("Done.")},
		llmtest.Turn{Chunks: llmtest.Text("unexpected")},
	)
	_, done := finished.queued("plot")

	// Act
	gotSecond := r.process(second)
	gotDone := finished.process(done)

	// Assert
	if gotSecond.Status != store.StatusCompleted || len(r.model.Calls()) != 3 {
		t.Fatalf("earlier plan: status %s calls %d", gotSecond.Status, len(r.model.Calls()))
	}
	if gotDone.Status != store.StatusCompleted || len(finished.model.Calls()) != 2 {
		t.Fatalf("finished plan: status %s calls %d", gotDone.Status, len(finished.model.Calls()))
	}
}

func TestProcessKeepsAcceptingSteersWhileAnAnswerNoteContinuesTheRun(t *testing.T) {
	// The answer checker's corrective note (dangling file link) is also decided before steers are closed.
	// Arrange
	r := newScriptedRig(t, false,
		llmtest.Turn{Chunks: llmtest.Text("See [x](workspace-file:///workspace/outputs/x.pdf)")},
		llmtest.Turn{Chunks: llmtest.Text("No file was produced.")},
	)
	steers := &countingSteers{SteerSource: r.q}
	r.agent.Steers = steers
	_, run := r.queued("pdf")

	// Act
	got := r.process(run)

	// Assert
	if got.Status != store.StatusCompleted || len(r.model.Calls()) != 2 || steers.finalizes.Load() != 1 {
		t.Fatalf("status %s calls %d finalizes %d", got.Status, len(r.model.Calls()), steers.finalizes.Load())
	}
}
