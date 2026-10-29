package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/compaction"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
)

// Cross-feature behaviour of the merged harness v2 (artifacts × goals, compaction, ask_user).

// commandWrites makes execute_command create the given file in the sandbox.
func commandWrites(r *scriptedRig, path, content string) {
	r.backend.OnCreate = func(s *sandboxtest.Sandbox) {
		s.SetExec(sandboxtest.FindExec(s, func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			s.Put(path, content)
			return sandbox.ExecResult{}, nil
		}))
	}
}

func TestGoalJudgeSeesTheArtifactsOfTheRun(t *testing.T) {
	// Files produced by a command and found by the end-of-run snapshot are listed for the goal judge; their events precede work.goal.
	// Arrange
	var sb *sandboxtest.Sandbox
	r := newScriptedRig(t, false,
		llmtest.Turn{Chunks: streamedCall("call_1", "execute_command", `{"command":"python build.py"}`)},
		llmtest.Turn{Chunks: llmtest.Text("The report is ready."), OnCall: func() { sb.Put("/workspace/chart.png", "png") }},
		llmtest.Turn{Chunks: llmtest.Text(`{"met": true, "reason": "report.pdf exists"}`)},
	)
	r.backend.OnCreate = func(s *sandboxtest.Sandbox) {
		sb = s
		s.SetExec(sandboxtest.FindExec(s, func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
			s.Put("/workspace/report.pdf", "%PDF")
			return sandbox.ExecResult{}, nil
		}))
	}
	th, _, run := r.goalRun(3)

	// Act
	r.process(run)

	// Assert
	calls := r.model.Calls()
	if len(calls) != 3 {
		t.Fatalf("%d model calls", len(calls))
	}
	judge := calls[2].Messages[1].Content
	if !strings.Contains(judge, "FILES PRODUCED IN THIS CONVERSATION:\n- /workspace/") ||
		!strings.Contains(judge, "/workspace/report.pdf") || !strings.Contains(judge, "/workspace/chart.png") {
		t.Fatalf("judge input %q", judge)
	}
	ts := types(r.events(run.ID))
	n := len(ts)
	if ts[n-1] != "RUN_FINISHED" || ts[n-2] != "CUSTOM:work.goal" || ts[n-3] != "CUSTOM:work.artifact" {
		t.Fatalf("types %v", ts)
	}
	if g := r.goal(th.ID); g.Status != store.GoalMet {
		t.Fatalf("goal %+v", g)
	}
}

func TestCompactionKeepsAttachmentsImagesAndArtifacts(t *testing.T) {
	// A manual compaction summarizes the attachment line and image of a vision message, lists command-made artifacts, and leaves metadata.artifacts intact.
	// Arrange
	r := newScriptedRig(t, true,
		llmtest.Turn{Chunks: streamedCall("call_1", "execute_command", `{"command":"python build.py"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Report ready")},
		llmtest.Turn{Chunks: llmtest.Text("## Goal\nA report on the chart.")},
	)
	commandWrites(r, "/workspace/report.docx", "docx")
	first := r.attachedRun(map[string][]byte{"chart.png": pngBytes})
	r.process(first)
	th := r.thread(first.ThreadID)
	compact := r.create(th, store.NewRun{Kind: store.KindCompact})

	// Act
	got := r.process(compact)

	// Assert
	calls := r.model.Calls()
	if got.Status != store.StatusCompleted || len(calls) != 3 {
		t.Fatalf("status %s, %d calls", got.Status, len(calls))
	}
	transcript := calls[2].Messages[1].Content
	if !strings.Contains(transcript, "USER: Look at this") || !strings.Contains(transcript, "[Attached files: /workspace/chart.png]") ||
		!strings.Contains(transcript, "[1 image attached]") {
		t.Fatalf("transcript %q", transcript)
	}
	sum, _ := r.st.GetThreadSummary(context.Background(), th.ID)
	if !strings.Contains(sum.Text, compaction.FilesHeading+"\n- /workspace/report.docx") {
		t.Fatalf("summary %q", sum.Text)
	}
	for _, m := range r.messages(th.ID) {
		if m.Role == store.RoleTool && m.ToolCallID == "call_1" && len(store.ArtifactPaths(m.Metadata)) == 1 {
			return
		}
	}
	t.Fatalf("metadata.artifacts lost: %+v", r.messages(th.ID))
}

func TestAskUserResumeWithArtifactsOnBothSides(t *testing.T) {
	// A file written before ask_user is reported before the interrupt; the resumed run replays the answer first and versions the file again.
	// Arrange
	r := newScriptedRig(t, false,
		llmtest.Turn{Chunks: []*schema.Message{
			llmtest.ToolCall(0, "call_w", "write_file", `{"path":"report.md","content":"# Draft"}`),
			llmtest.ToolCall(1, "call_ask", "ask_user", askArgs),
		}},
		llmtest.Turn{Chunks: streamedCall("call_x", "execute_command", `{"command":"render report.md"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Here is the PDF version")},
	)
	commandWrites(r, "/workspace/report.md", "# Final, as PDF")
	th, first := r.queued("make the report")
	r.process(first)
	q := r.thread(th.ID).PendingQuestion
	second := r.create(th, store.NewRun{Resume: &store.Resume{InterruptID: q.InterruptID, Content: "The user chose: PDF"}})

	// Act
	got := r.process(second)

	// Assert
	firstEvs := r.events(first.ID)
	ft := types(firstEvs)
	res := indexOf(firstEvs, "TOOL_CALL_RESULT")
	if ft[res+1] != "CUSTOM:work.artifact" || ft[len(ft)-1] != "RUN_FINISHED" || index(ft, "STEP_FINISHED") < res+1 ||
		last(firstEvs)["outcome"].(map[string]any)["type"] != "interrupt" {
		t.Fatalf("first run %v", ft)
	}
	evs := r.events(second.ID)
	ts := types(evs)
	if got.Status != store.StatusCompleted || ts[1] != "TOOL_CALL_RESULT" || evs[1]["toolCallId"] != "call_ask" {
		t.Fatalf("second run %s %v", got.Status, ts)
	}
	arts := find(evs, "CUSTOM:work.artifact")
	if len(arts) != 1 {
		t.Fatalf("artifact events %v", ts)
	}
	v := arts[0]["value"].(map[string]any)
	if v["toolCallId"] != "call_x" || v["change"] != "updated" || v["version"] != float64(2) {
		t.Fatalf("artifact event %v", v)
	}
	sent := r.model.Calls()[1].Messages
	var roles []string
	for _, m := range sent[1:] {
		roles = append(roles, string(m.Role)+":"+m.ToolCallID)
	}
	if !equal(roles, []string{"user:", "assistant:", "tool:call_w", "tool:call_ask"}) || sent[len(sent)-1].Content != "The user chose: PDF" {
		t.Fatalf("history %v", roles)
	}
}
