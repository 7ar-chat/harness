package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/features/goals"
	"github.com/7ar-chat/harness/features/titles"
	"github.com/7ar-chat/harness/features/verify"
)

// judgeRig is a verification rig whose goal judge gets the verifier's evidence.
func judgeRig(t *testing.T, turns ...llmtest.Turn) *scriptedRig {
	t.Helper()
	r := verifyRig(t, false, turns...) // strict AG-UI order (verifyRig)
	ev := verify.New(verify.Options{Sandbox: r.mgr, Messages: r.st})
	for _, h := range r.opts.Hooks {
		if g, ok := h.(*goals.Hook); ok {
			g.Evidence = ev.Evidence
		}
	}
	return r
}

func goalOn(t *testing.T, r *scriptedRig, text string) (*store.Thread, *store.Run) {
	t.Helper()
	ctx := context.Background()
	th, _ := r.st.CreateThread(ctx, "alice", "t")
	g, err := r.st.PutGoal(ctx, "alice", th.ID, text, 3)
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := r.st.CreateRun(ctx, goals.NewRun(g, "alice", "gpt-test", "", "", goals.StartMessage(g.Text)))
	if err != nil {
		t.Fatal(err)
	}
	return th, run
}

func TestGoalJudgeSeesDeliverableTextAndCheckerResults(t *testing.T) {
	// The goal judge gets each deliverable of the turn with its checker result and the start of its text, fenced as data.
	// Arrange
	r := judgeRig(t,
		llmtest.Turn{Chunks: streamedCall("w1", "write_file", `{"path":"outputs/summary.md","content":"# Q3\nRevenue 12.4 M EUR"}`)},
		llmtest.Turn{Chunks: streamedCall("w2", "write_file", `{"path":"outputs/report.docx","content":"fine"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Both files are ready.")},
		llmtest.Turn{Chunks: llmtest.Text(`{"met": true, "reason": "Both written.", "blockers": []}`)},
	)
	r.agent.MaxSteps = 10
	_, run := goalOn(t, r, "Write the Q3 summary and report")

	// Act
	r.process(run)

	// Assert
	calls := r.model.Calls()
	judge := calls[len(calls)-1].Messages[1].Content
	for _, want := range []string{"DELIVERABLES OF THE GOAL'S TURNS", "/workspace/outputs/report.docx — checker passed: inspect_docx: ok (2 checks passed)",
		"/workspace/outputs/summary.md — no checker for this file type", "<deliverable_text>\n# Q3\nRevenue 12.4 M EUR\n</deliverable_text>"} {
		if !strings.Contains(judge, want) {
			t.Fatalf("judge input misses %q:\n%s", want, judge)
		}
	}
}

func TestGoalIsNeverMetWhileADeliverableCheckerFails(t *testing.T) {
	// A judge saying "met" while a deliverable's checker fails is overruled: the goal stays active, the verdict has the failure as a blocker and the next goal run lists it.
	// Arrange
	r := judgeRig(t,
		llmtest.Turn{Chunks: streamedCall("w1", "write_file", `{"path":"outputs/report.docx","content":"BROKEN"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done.")},
		llmtest.Turn{Chunks: llmtest.Text("Done, headings unfixed.")},
		llmtest.Turn{Chunks: llmtest.Text(`{"met": true, "reason": "The report exists."}`)},
	)
	r.agent.MaxSteps = 10
	th, run := goalOn(t, r, "Write the report")

	// Act
	r.process(run)

	// Assert
	g, _ := r.st.ThreadGoal(context.Background(), th.ID)
	if g.Status != store.GoalActive || g.LastVerdict == nil || g.LastVerdict.Met || len(g.LastVerdict.Blockers) != 1 ||
		!strings.Contains(g.LastVerdict.Blockers[0], "/workspace/outputs/report.docx: inspect_docx: NOT OK") {
		t.Fatalf("goal %+v verdict %+v", g, g.LastVerdict)
	}
	ev := goalEvent(t, r.events(run.ID))
	lv := ev["lastVerdict"].(map[string]any)
	if lv["met"] != false || len(lv["blockers"].([]any)) != 1 || ev["nextRunId"] == nil {
		t.Fatalf("event %v", ev)
	}
	msgs := r.messages(th.ID)
	if next := msgs[len(msgs)-1]; next.Role != store.RoleUser || !strings.Contains(next.Content, "Still missing:\n- /workspace/outputs/report.docx") {
		t.Fatalf("next goal message %+v", next)
	}
}

func TestGoalVerdictIsFoundAmongTextAndBraces(t *testing.T) {
	// The judge's JSON is found wherever it is, with braces in the surrounding text; its blockers are kept (A-22).
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: llmtest.Text("partial")},
		llmtest.Turn{Chunks: llmtest.Text(`Thinking about {the goal}... {"met": "false", "reason": "No chart.", "blockers": ["the chart is missing", "  "]} (I checked the {files} list.)`)},
	)
	th, _, run := r.goalRun(3)

	// Act
	r.process(run)

	// Assert
	g := r.goal(th.ID)
	if g.LastVerdict == nil || g.LastVerdict.Reason != "No chart." || g.LastVerdict.Met || len(g.LastVerdict.Blockers) != 1 ||
		g.LastVerdict.Blockers[0] != "the chart is missing" || g.EvalFailures != 0 {
		t.Fatalf("verdict %+v failures %d", g.LastVerdict, g.EvalFailures)
	}
}

func TestAuxiliaryCallsAreCountedInTheRunUsageAndRunAtLowEffort(t *testing.T) {
	// The title and goal-judge calls run at low effort on reasoning models and their tokens are part of RUN_FINISHED.usage (A-25).
	// Arrange
	withUsage := func(text string, in, out int) llmtest.Turn {
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.WithUsage(&ai.Message{Role: ai.RoleAssistant, Content: text}, in, out)}}
	}
	r := newRig(t, withUsage("Here is the report.", 100, 10), withUsage("Q3 report", 20, 3), withUsage(`{"met": true, "reason": "ok"}`, 50, 5))
	for _, h := range r.opts.Hooks {
		switch x := h.(type) {
		case *titles.Hook:
			x.SupportsReasoning = func(m string) bool { return m == "gpt-reasoning" }
		case *goals.Hook:
			x.SupportsReasoning = func(m string) bool { return m == "gpt-reasoning" }
		}
	}
	ctx := context.Background()
	th, _ := r.st.CreateThread(ctx, "alice", "")
	g, _ := r.st.PutGoal(ctx, "alice", th.ID, "Write the Q3 report", 3)
	run, _, err := r.st.CreateRun(ctx, goals.NewRun(g, "alice", "gpt-reasoning", "high", "", goals.StartMessage(g.Text)))
	if err != nil {
		t.Fatal(err)
	}

	// Act
	r.process(run)

	// Assert
	calls := r.model.Calls()
	if len(calls) != 3 || calls[0].ReasoningEffort != "high" || calls[1].ReasoningEffort != "low" || calls[2].ReasoningEffort != "low" {
		t.Fatalf("efforts %d calls", len(calls))
	}
	usage := last(r.events(run.ID))["usage"].([]any)
	u := usage[0].(map[string]any)
	if len(usage) != 1 || u["model"] != "gpt-reasoning" || u["inputTokens"] != 170.0 || u["outputTokens"] != 18.0 {
		t.Fatalf("usage %v", usage)
	}
}
