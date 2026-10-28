package runtime_test

import (
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/store"
)

func TestGoalJudgeSeesTheDeliverablesOfEveryGoalTurn(t *testing.T) {
	// A deliverable that failed its checker in an earlier goal turn and was not fixed since is still evidence (and a blocker) when a later goal turn is judged.
	// Arrange
	r := judgeRig(t,
		llmtest.Turn{Chunks: streamedCall("w1", "write_file", `{"path":"outputs/report.docx","content":"BROKEN"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Report drafted.")},
		llmtest.Turn{Chunks: llmtest.Text("Report drafted; its headings still fail.")},
		llmtest.Turn{Chunks: llmtest.Text(`{"met": false, "reason": "The summary is missing.", "blockers": ["summary.docx"]}`)},
		llmtest.Turn{Chunks: streamedCall("w2", "write_file", `{"path":"outputs/summary.docx","content":"fine"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Summary written.")},
		llmtest.Turn{Chunks: llmtest.Text(`{"met": true, "reason": "Both exist."}`)},
	)
	r.agent.MaxSteps = 10
	th, first := goalOn(t, r, "Write the report and its summary")
	r.process(first)
	msgs := r.messages(th.ID)
	second := r.mustRun(msgs[len(msgs)-1].RunID)

	// Act
	r.process(second)

	// Assert
	calls := r.model.Calls()
	judge := calls[len(calls)-1].Messages[1].Content
	for _, want := range []string{"/workspace/outputs/report.docx — CHECKER FAILED", "/workspace/outputs/summary.docx — checker passed"} {
		if !strings.Contains(judge, want) {
			t.Fatalf("the second judge misses %q:\n%s", want, judge)
		}
	}
	if g := r.goal(th.ID); g.Status != store.GoalActive || g.LastVerdict == nil || g.LastVerdict.Met || len(g.LastVerdict.Blockers) == 0 {
		t.Fatalf("goal %+v verdict %+v", g, g.LastVerdict)
	}
}
