package runtime_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
)

// lsCall is a list_files call of path.
func lsCall(id, path string) llmtest.Turn {
	return llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, id, "list_files", `{"path":"`+path+`"}`)}}
}

func TestProcessNotesACallThatKeepsReturningTheSameResult(t *testing.T) {
	// A call made for the 3rd time in a task with the same result (a re-reading cycle no consecutive guard sees: A-B-C-A-B-C-A) gets a note telling the model to use what it has instead of running it again (A2).
	// Arrange
	var turns []llmtest.Turn
	for i, n := range []int{1, 2, 3, 1, 2, 3, 1} {
		turns = append(turns, llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, fmt.Sprintf("c%d", i), "dump", fmt.Sprintf(`{"n":%d}`, n))}})
	}
	r := newRig(t, append(turns, llmtest.Turn{Chunks: llmtest.Text("done")})...)
	r.agent.MaxSteps = 10
	r.withTools(dumpTool())
	_, run := r.queued("read the reports")

	// Act
	got := r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	results := find(r.events(run.ID), "TOOL_CALL_RESULT")
	if got.Status != store.StatusCompleted || len(results) != 7 {
		t.Fatalf("status %s results %d", got.Status, len(results))
	}
	for i, e := range results {
		noted := strings.Contains(e["content"].(string), "[note] You already ran this exact call 3 times")
		if noted != (i == 6) {
			t.Fatalf("result %d noted=%v: …%s", i, noted, e["content"].(string)[max(0, len(e["content"].(string))-200):])
		}
	}
}

func TestProcessPushesForAPartialDeliverableBeforeTheLastSteps(t *testing.T) {
	// A run that still has no deliverable when only max(3, 10 %) of its steps are left is told once to write what it has to /workspace/outputs/ now (A2).
	// Arrange
	var turns []llmtest.Turn
	for i := 1; i < 20; i++ {
		turns = append(turns, lsCall(fmt.Sprintf("c%d", i), fmt.Sprintf("/workspace/d%d", i)))
	}
	r := newRig(t, append(turns, llmtest.Turn{Chunks: llmtest.Text("Here is what I found so far.")})...)
	r.agent.MaxSteps = 20
	_, run := r.queued("write the synthesis")

	// Act
	r.process(run)

	// Assert
	calls := r.model.Calls()
	if len(calls) != 20 {
		t.Fatalf("calls %d", len(calls))
	}
	for i, c := range calls {
		pushes := 0
		for _, m := range c.Messages {
			if m.Role == schema.User && strings.Contains(m.Content, "Only 3 steps left and no deliverable has been written yet") {
				pushes++
			}
		}
		if want := map[bool]int{true: 1, false: 0}[i >= 17]; pushes != want { // step 18 of 20: 3 steps left
			t.Fatalf("call %d: %d pushes, want %d", i, pushes, want)
		}
	}
}

func TestProcessEndsWithMaxStepsWhenTheBudgetRunsOutWithoutTheFileItWasAskedFor(t *testing.T) {
	// A run that was going to write a file (its plan, its own words or a call names /workspace/outputs/) and is forced to answer on its last step without having written one ends with RUN_ERROR max_steps (not a success); its answer is kept (A2, F3).
	for name, first := range map[string][]*schema.Message{
		"plan":    {llmtest.ToolCall(0, "c1", "update_plan", `{"items":[{"id":"1","title":"Write /workspace/outputs/synthesis.docx","status":"in_progress"}]}`)},
		"said":    append(llmtest.Text("I will write the synthesis to /workspace/outputs/synthesis.docx."), llmtest.ToolCall(0, "c1", "list_files", `{"path":"/workspace"}`)),
		"started": {llmtest.ToolCall(0, "c1", "execute_command", `{"command":"mkdir -p /workspace/outputs && python3 /workspace/.work/make_synthesis.py"}`)},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			r := newRig(t, llmtest.Turn{Chunks: first}, llmtest.Turn{Chunks: llmtest.Text("I could not finish: here is what I found.")})
			r.agent.MaxSteps = 2
			th, run := r.queued("write the synthesis")

			// Act
			got := r.process(run)

			// Assert
			evs := r.events(run.ID)
			checkStrictOrder(t, evs)
			fin := last(evs)
			if got.Status != store.StatusFailed || fin["type"] != "RUN_ERROR" || fin["code"] != "max_steps" {
				t.Fatalf("status %s last %v", got.Status, fin)
			}
			if a := lastAnswer(t, &scriptedRig{rig: r}, th.ID); a.Content != "I could not finish: here is what I found." {
				t.Fatalf("answer %q", a.Content)
			}
			if calls := r.model.Calls(); calls[1].Messages[len(calls[1].Messages)-1].Content != agent.StepLimitNudge {
				t.Fatalf("no step-limit nudge")
			}
		})
	}
}

func TestProcessCompletesAQuestionWhoseBudgetRanOutWithoutAFile(t *testing.T) {
	// A question or analysis run that never meant to write a file and is forced to answer on its last step completes with that answer: no deliverable was asked for (F3).
	// Arrange
	r := newRig(t, lsCall("c1", "/workspace"), llmtest.Turn{Chunks: llmtest.Text("The folder holds three reports.")})
	r.agent.MaxSteps = 2
	_, run := r.queued("what is in the folder?")

	// Act
	got := r.process(run)

	// Assert
	if fin := last(r.events(run.ID)); got.Status != store.StatusCompleted || fin["type"] != "RUN_FINISHED" {
		t.Fatalf("status %s last %v", got.Status, fin)
	}
}

func TestProcessCompletesARunThatChangedAnUploadInPlaceBeforeItsBudgetRanOut(t *testing.T) {
	// A run that edited a file the user uploaded where it is (outside /workspace/outputs/) produced its result: forced to answer on its last step, it completes (F3).
	// Arrange
	r := newScriptedRig(t, false,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "w1", "write_file", `{"path":"/workspace/notes.md","content":"# Notes, fixed"}`)}},
		llmtest.Turn{Chunks: llmtest.Text("Notes fixed in place.")})
	r.agent.MaxSteps = 2
	run := r.attachedRunWith("fix the typos in my notes", map[string][]byte{"notes.md": []byte("# Ntoes")})

	// Act
	got := r.process(run)

	// Assert
	if fin := last(r.events(run.ID)); got.Status != store.StatusCompleted || fin["type"] != "RUN_FINISHED" {
		t.Fatalf("status %s last %v", got.Status, fin)
	}
}

func TestProcessCompletesWhenTheLastStepAnswersAfterWritingADeliverable(t *testing.T) {
	// A model that wrote a deliverable and answers on the last (tool-less) step completes the run.
	// Arrange
	r := newScriptedRig(t, false,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "w1", "write_file", `{"path":"/workspace/outputs/notes.md","content":"# Notes"}`)}},
		llmtest.Turn{Chunks: llmtest.Text("Notes written.")})
	r.agent.MaxSteps = 2
	_, run := r.queued("write notes")

	// Act
	got := r.process(run)

	// Assert
	if got.Status != store.StatusCompleted {
		t.Fatalf("status %s", got.Status)
	}
}
