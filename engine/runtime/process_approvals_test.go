package runtime_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/approvals"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/engine/tools"
)

const writeArgs = `{"path":"/workspace/report.md","content":"# Report"}`

// newApprovalRig wires the approval gate like cmd/work.
func newApprovalRig(t *testing.T, turns ...llmtest.Turn) *rig {
	t.Helper()
	r := newRig(t, turns...)
	r.strictOrder()
	r.agent.Harness.BeforeTool = []agent.BeforeToolHook{&approvals.Gate{Store: r.st, Resolver: approvals.SandboxResolver{Sandboxes: r.mgr}}}
	r.agent.Harness.Approvals = r.st
	return r
}

// interactive creates a thread with a queued interactive run.
func (r *rig) interactive(content string) (*store.Thread, *store.Run) {
	r.t.Helper()
	th, err := r.st.CreateThread(context.Background(), "alice", "t")
	if err != nil {
		r.t.Fatal(err)
	}
	return th, r.create(th, store.NewRun{Message: store.Message{Role: store.RoleUser, Content: content}, InteractionMode: store.InteractionInteractive})
}

// decide creates the resume run of the thread's pending approval.
func (r *rig) decide(th *store.Thread, decision string, resp store.ApprovalResponse) *store.Run {
	r.t.Helper()
	a := r.thread(th.ID).PendingApproval
	if a == nil {
		r.t.Fatal("no pending approval")
	}
	return r.create(th, store.NewRun{Resume: &store.Resume{InterruptID: a.ID, Decision: decision, Response: resp}})
}

func (r *rig) file(p string) (string, bool) {
	created := r.backend.Created()
	if len(created) == 0 {
		return "", false
	}
	return created[len(created)-1].File(p)
}

// approvalTestTools adds tools to the rig's registry.
type approvalTestTools []tool.BaseTool

func (s approvalTestTools) Name() string { return "static" }
func (s approvalTestTools) Tools(context.Context, tools.RunContext) ([]tool.BaseTool, func(), error) {
	return s, nil, nil
}

func addTools(t *testing.T, r *rig, ts ...tool.BaseTool) {
	t.Helper()
	r.agent.Tools = tools.NewRegistry(r.agent.Tools, approvalTestTools(ts))
}

type noArgs struct{}

func scriptedTool(t *testing.T, name string, fn func(ctx context.Context) (string, error)) tool.BaseTool {
	t.Helper()
	tl, err := utils.InferTool(name, "test tool", func(ctx context.Context, _ noArgs) (string, error) { return fn(ctx) })
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

func approvalEvents(evs []event) []map[string]any {
	var out []map[string]any
	for _, e := range find(evs, "CUSTOM:work.approval") {
		out = append(out, e["value"].(map[string]any))
	}
	return out
}

func TestProcessAutomaticModeRunsRiskyToolsWithoutApproval(t *testing.T) {
	// Automatic runs (the API default) are unchanged: a write_file call runs at once, nothing waits for a decision.
	// Arrange
	r := newApprovalRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "w1", "write_file", writeArgs)}},
		llmtest.Turn{Chunks: llmtest.Text("Written")},
	)
	th, run := r.queued("write the report")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	if got.Status != store.StatusCompleted || got.InteractionMode != store.InteractionAutomatic || len(approvalEvents(evs)) != 0 ||
		len(find(evs, "TOOL_CALL_RESULT")) != 1 || last(evs)["outcome"].(map[string]any)["type"] != "success" {
		t.Fatalf("status %s mode %q types %v", got.Status, got.InteractionMode, types(evs))
	}
	if content, ok := r.file("/workspace/report.md"); !ok || content != "# Report" {
		t.Fatalf("file %q %v", content, ok)
	}
	if r.thread(th.ID).PendingApproval != nil || len(r.st.ApprovalsOf(th.ID)) != 0 {
		t.Fatal("approval recorded in automatic mode")
	}
}

func TestProcessInteractiveRiskyCallPausesBeforeExecuting(t *testing.T) {
	// In interactive mode a write_file call is not run: the approval is persisted, work.approval required is emitted and the run ends with a tool_approval interrupt.
	// Arrange
	r := newApprovalRig(t, llmtest.Turn{Chunks: append(llmtest.Text("Writing it."), llmtest.ToolCall(0, "w1", "write_file", writeArgs))})
	th, run := r.interactive("write the report")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	want := []string{"RUN_STARTED", "STEP_STARTED", "TEXT_MESSAGE_START", "TEXT_MESSAGE_CONTENT", "TEXT_MESSAGE_END", "TOOL_CALL_START", "TOOL_CALL_ARGS",
		"TOOL_CALL_END", "CUSTOM:work.approval", "CUSTOM:work.usage", "STEP_FINISHED", "RUN_FINISHED"}
	if got.Status != store.StatusCompleted || got.InteractionMode != store.InteractionInteractive || !equal(types(evs), want) || len(r.model.Calls()) != 1 {
		t.Fatalf("status %s types %v calls %d", got.Status, types(evs), len(r.model.Calls()))
	}
	if _, ok := r.file("/workspace/report.md"); ok {
		t.Fatal("the risky call ran before its approval")
	}
	pending := r.thread(th.ID).PendingApproval
	wantPreview := `{"content":"# Report","path":"/workspace/report.md"}`
	if pending == nil || pending.RunID != run.ID || pending.ToolCallID != "w1" || pending.ToolName != "write_file" ||
		pending.ToolInputPreview != wantPreview || pending.Status != store.ApprovalPending || pending.Kind != store.ApprovalKindRiskyTool {
		t.Fatalf("pending %+v", pending)
	}
	required := approvalEvents(evs)[0]
	payload := required["approval"].(map[string]any)
	if required["phase"] != "required" || payload["id"] != pending.ID || payload["toolInputPreview"] != wantPreview ||
		payload["status"] != "pending" || payload["decision"] != nil || payload["createdAt"] == "" {
		t.Fatalf("required %v", required)
	}
	outcome := last(evs)["outcome"].(map[string]any)
	intr := outcome["interrupts"].([]any)[0].(map[string]any)
	if outcome["type"] != "interrupt" || intr["reason"] != "tool_approval" || intr["id"] != pending.ID || intr["toolCallId"] != "w1" ||
		intr["message"] != "write_file" || intr["approval"].(map[string]any)["toolName"] != "write_file" {
		t.Fatalf("outcome %v", outcome)
	}
	if r.thread(th.ID).PendingQuestion != nil {
		t.Fatal("approval recorded as a question")
	}
}

func TestProcessApproveExecutesTheExactPendingCall(t *testing.T) {
	// approve: the resume run announces the decision, runs the paused call with its exact arguments and gives the model its real result.
	// Arrange
	r := newApprovalRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "w1", "write_file", writeArgs)}},
		llmtest.Turn{Chunks: llmtest.Text("Report written")},
	)
	th, first := r.interactive("write the report")
	r.process(first)
	second := r.decide(th, store.DecisionApprove, store.ApprovalResponse{})

	// Act
	got := r.process(second)

	// Assert
	evs := r.events(second.ID)
	ty := types(evs)
	if got.Status != store.StatusCompleted || ty[1] != "CUSTOM:work.approval" || ty[2] != "TOOL_CALL_RESULT" || ty[3] != "STEP_STARTED" {
		t.Fatalf("status %s types %v", got.Status, ty)
	}
	resolved := approvalEvents(evs)[0]
	if resolved["phase"] != "resolved" || resolved["approval"].(map[string]any)["decision"] != "approve" ||
		resolved["approval"].(map[string]any)["status"] != "resolved" {
		t.Fatalf("resolved %v", resolved)
	}
	if res := evs[2]; res["toolCallId"] != "w1" || !strings.Contains(res["content"].(string), `"bytesWritten":8`) || res["metadata"] != nil {
		t.Fatalf("result %v", res)
	}
	if content, ok := r.file("/workspace/report.md"); !ok || content != "# Report" {
		t.Fatalf("file %q %v", content, ok)
	}
	msgs := r.model.Calls()[1].Messages
	n := len(msgs)
	if msgs[n-2].Role != schema.Assistant || msgs[n-2].ToolCalls[0].ID != "w1" || msgs[n-1].Role != schema.Tool ||
		msgs[n-1].ToolCallID != "w1" || !strings.Contains(msgs[n-1].Content, "bytesWritten") {
		t.Fatalf("history %+v", msgs[1:])
	}
	if r.thread(th.ID).PendingApproval != nil {
		t.Fatal("approval still pending")
	}
}

func TestProcessDenyAndRespondGiveWorksTexts(t *testing.T) {
	// deny and respond never run the call; the model gets Work's instruction texts as the tool result.
	cases := []struct {
		name     string
		decision string
		resp     store.ApprovalResponse
		want     string
	}{
		{"deny", store.DecisionDeny, store.ApprovalResponse{},
			"The user denied the pending action. Do not retry it. Choose a safe alternative and continue."},
		{"respond with text", store.DecisionRespond, store.ApprovalResponse{ResponseText: "write it to /workspace/draft.md"},
			"The user answered the pending question. Use this answer and continue: free text: write it to /workspace/draft.md."},
		{"respond with choice and text", store.DecisionRespond, store.ApprovalResponse{SelectedChoice: "Draft", ResponseText: "keep it short"},
			"The user answered the pending question. Use this answer and continue: selected option: Draft; free text: keep it short."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			r := newApprovalRig(t,
				llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "w1", "write_file", writeArgs)}},
				llmtest.Turn{Chunks: llmtest.Text("OK")},
			)
			th, first := r.interactive("write the report")
			r.process(first)
			second := r.decide(th, tc.decision, tc.resp)

			// Act
			got := r.process(second)

			// Assert
			evs := r.events(second.ID)
			results := find(evs, "TOOL_CALL_RESULT")
			if got.Status != store.StatusCompleted || len(results) != 1 || results[0]["toolCallId"] != "w1" || results[0]["content"] != tc.want {
				t.Fatalf("status %s results %v", got.Status, results)
			}
			if _, ok := r.file("/workspace/report.md"); ok {
				t.Fatal("a refused call ran")
			}
			msgs := r.model.Calls()[1].Messages
			if last := msgs[len(msgs)-1]; last.Role != schema.Tool || last.Content != tc.want {
				t.Fatalf("model saw %+v", last)
			}
			if a := approvalEvents(evs)[0]["approval"].(map[string]any); a["decision"] != tc.decision {
				t.Fatalf("resolved %v", a)
			}
		})
	}
}

// tamperSource serves the resumed approval with another tool input (the arguments changed).
type tamperSource struct {
	src agent.ApprovalSource
}

func (s tamperSource) ResumedApproval(ctx context.Context, runID string) (*store.Approval, error) {
	a, err := s.src.ResumedApproval(ctx, runID)
	if a != nil {
		a.ToolInputPreview = `{"content":"# Other","path":"/workspace/report.md"}`
	}
	return a, err
}

func TestProcessApproveRefusesACallWhoseArgumentsChanged(t *testing.T) {
	// An approval only runs the call it describes: changed arguments get an error result and the call is not run.
	// Arrange
	r := newApprovalRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "w1", "write_file", writeArgs)}},
		llmtest.Turn{Chunks: llmtest.Text("OK")},
	)
	th, first := r.interactive("write the report")
	r.process(first)
	second := r.decide(th, store.DecisionApprove, store.ApprovalResponse{})
	r.agent.Harness.Approvals = tamperSource{src: r.st}

	// Act
	r.process(second)

	// Assert
	results := find(r.events(second.ID), "TOOL_CALL_RESULT")
	if len(results) != 1 || results[0]["content"] != approvals.MismatchResult || results[0]["metadata"].(map[string]any)["isError"] != true {
		t.Fatalf("results %v", results)
	}
	if _, ok := r.file("/workspace/report.md"); ok {
		t.Fatal("the changed call ran")
	}
}

func TestProcessRiskyCallsOfOneStepAreApprovedOneAtATime(t *testing.T) {
	// [write_file, list_files, execute_command]: the first risky call pauses, list_files runs, execute_command waits;
	// approving the first runs it and pauses on the second without a model call; approving the second continues the loop.
	// Arrange
	r := newApprovalRig(t,
		llmtest.Turn{Chunks: []*schema.Message{
			llmtest.ToolCall(0, "w1", "write_file", writeArgs),
			llmtest.ToolCall(1, "l1", "list_files", `{"path":"/workspace"}`),
			llmtest.ToolCall(2, "x1", "execute_command", `{"command":"wc -c report.md"}`)}},
		llmtest.Turn{Chunks: llmtest.Text("All done")},
	)
	th, first := r.interactive("report + count")

	// Act
	r.process(first)
	firstResults := find(r.events(first.ID), "TOOL_CALL_RESULT")
	pendingW := r.thread(th.ID).PendingApproval
	second := r.process(r.decide(th, store.DecisionApprove, store.ApprovalResponse{}))
	secondEvs := r.events(second.ID)
	pendingX := r.thread(th.ID).PendingApproval
	third := r.process(r.decide(th, store.DecisionApprove, store.ApprovalResponse{}))

	// Assert
	if len(firstResults) != 1 || firstResults[0]["toolCallId"] != "l1" || pendingW == nil || pendingW.ToolCallID != "w1" {
		t.Fatalf("first run: results %v pending %+v", firstResults, pendingW)
	}
	wantSecond := []string{"RUN_STARTED", "CUSTOM:work.approval", "TOOL_CALL_RESULT", "CUSTOM:work.approval", "RUN_FINISHED"}
	if !equal(types(secondEvs), wantSecond) || secondEvs[2]["toolCallId"] != "w1" || pendingX == nil || pendingX.ToolCallID != "x1" ||
		pendingX.RunID != second.ID || approvalEvents(secondEvs)[1]["phase"] != "required" {
		t.Fatalf("second run: types %v pending %+v", types(secondEvs), pendingX)
	}
	if intr := last(secondEvs)["outcome"].(map[string]any)["interrupts"].([]any)[0].(map[string]any); intr["toolCallId"] != "x1" {
		t.Fatalf("second interrupt %v", intr)
	}
	if third.Status != store.StatusCompleted || len(r.model.Calls()) != 2 {
		t.Fatalf("third %s calls %d", third.Status, len(r.model.Calls()))
	}
	sb := r.backend.Created()[0]
	if execs := sb.Execs(); len(execs) == 0 || execs[len(execs)-1].Command != "wc -c report.md" {
		t.Fatalf("execs %+v", execs)
	}
	var got []string
	for _, m := range r.model.Calls()[1].Messages[1:] {
		got = append(got, string(m.Role)+":"+m.ToolCallID)
	}
	want := []string{"user:", "assistant:", "tool:w1", "tool:l1", "tool:x1"}
	if !equal(got, want) {
		t.Fatalf("history\n got %q\nwant %q", got, want)
	}
}

func TestProcessMCPDeleteToolIsGated(t *testing.T) {
	// MCP tools ({server}__{tool}) whose tool part starts with delete/remove/move/rename need approval; others run.
	// Arrange
	var deleted, listed bool
	r := newApprovalRig(t, llmtest.Turn{Chunks: []*schema.Message{
		llmtest.ToolCall(0, "m1", "jira__list_issues", `{}`), llmtest.ToolCall(1, "m2", "jira__delete_issue", `{}`)}})
	addTools(t, r,
		scriptedTool(t, "jira__list_issues", func(context.Context) (string, error) { listed = true; return "[]", nil }),
		scriptedTool(t, "jira__delete_issue", func(context.Context) (string, error) { deleted = true; return "deleted", nil }))
	th, run := r.interactive("clean up jira")

	// Act
	r.process(run)

	// Assert
	if !listed || deleted {
		t.Fatalf("listed %v deleted %v", listed, deleted)
	}
	if a := r.thread(th.ID).PendingApproval; a == nil || a.ToolName != "jira__delete_issue" || a.ToolInputPreview != "{}" {
		t.Fatalf("pending %+v", a)
	}
}

func TestProcessAskUserAfterAPausedCallWaitsForTheDecision(t *testing.T) {
	// An ask_user call after a paused risky call is deferred: after approval the call runs, then the question interrupts.
	// Arrange
	r := newApprovalRig(t, llmtest.Turn{Chunks: []*schema.Message{
		llmtest.ToolCall(0, "w1", "write_file", writeArgs), llmtest.ToolCall(1, "q1", "ask_user", `{"question":"Which title?"}`)}})
	th, first := r.interactive("report")
	r.process(first)
	firstResults := find(r.events(first.ID), "TOOL_CALL_RESULT")

	// Act
	second := r.process(r.decide(th, store.DecisionApprove, store.ApprovalResponse{}))

	// Assert
	if len(firstResults) != 0 {
		t.Fatalf("first run results %v", firstResults)
	}
	q := r.thread(th.ID).PendingQuestion
	outcome := last(r.events(second.ID))["outcome"].(map[string]any)
	if q == nil || q.ToolCallID != "q1" || q.RunID != second.ID || outcome["interrupts"].([]any)[0].(map[string]any)["reason"] != "ask_user" {
		t.Fatalf("question %+v outcome %v", q, outcome)
	}
	if _, ok := r.file("/workspace/report.md"); !ok {
		t.Fatal("approved call did not run")
	}
}

func TestProcessNewMessageCancelsThePendingApproval(t *testing.T) {
	// A new message instead of a decision cancels the approval: the call is not run, the model sees why, deferred calls stay unrun.
	// Arrange
	r := newApprovalRig(t,
		llmtest.Turn{Chunks: []*schema.Message{
			llmtest.ToolCall(0, "w1", "write_file", writeArgs), llmtest.ToolCall(1, "x1", "execute_command", `{"command":"ls"}`)}},
		llmtest.Turn{Chunks: llmtest.Text("OK, something else")},
	)
	th, first := r.interactive("report")
	r.process(first)
	approval := r.thread(th.ID).PendingApproval
	second := r.create(th, store.NewRun{Message: store.Message{Role: store.RoleUser, Content: "never mind"}})

	// Act
	r.process(second)

	// Assert
	evs := r.events(second.ID)
	resolved := approvalEvents(evs)
	if len(resolved) != 1 || resolved[0]["phase"] != "resolved" || resolved[0]["approval"].(map[string]any)["status"] != "cancelled" {
		t.Fatalf("approval events %v", resolved)
	}
	if got, _ := r.st.GetApproval(context.Background(), approval.ID); got.Status != store.ApprovalCancelled || got.ResumeRunID != second.ID {
		t.Fatalf("approval %+v", got)
	}
	var got []string
	for _, m := range r.model.Calls()[1].Messages[1:] {
		got = append(got, string(m.Role)+":"+m.Content)
	}
	want := []string{"user:report", "assistant:", "tool:" + store.ImplicitApprovalCancelResult,
		"tool:The tool call was interrupted before it returned a result.", "user:never mind"}
	if !equal(got, want) {
		t.Fatalf("history\n got %q\nwant %q", got, want)
	}
	if _, ok := r.file("/workspace/report.md"); ok || r.thread(th.ID).PendingApproval != nil {
		t.Fatal("cancelled call ran or approval still pending")
	}
}

func TestProcessCancelledRunDoesNotKeepItsApproval(t *testing.T) {
	// A run cancelled after it paused (a later call of the step was still running) leaves no pending approval.
	// Arrange
	started := make(chan struct{})
	r := newApprovalRig(t, llmtest.Turn{Chunks: []*schema.Message{
		llmtest.ToolCall(0, "w1", "write_file", writeArgs), llmtest.ToolCall(1, "s1", "slow_lookup", `{}`)}})
	addTools(t, r, scriptedTool(t, "slow_lookup", func(ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}))
	th, run := r.interactive("report")
	done := make(chan *store.Run)
	go func() { done <- r.process(run) }()
	<-started

	// Act
	_, _ = r.st.RequestCancel(context.Background(), run.ID)
	_ = r.q.PublishCancel(context.Background(), run.ID)

	// Assert
	var got *store.Run
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop")
	}
	list := r.st.ApprovalsOf(th.ID)
	if got.Status != store.StatusCancelled || len(list) != 1 || list[0].Status != store.ApprovalCancelled || r.thread(th.ID).PendingApproval != nil {
		t.Fatalf("status %s approvals %+v", got.Status, list)
	}
}

func TestProcessNextRunAnnouncesAnApprovalCancelledByStop(t *testing.T) {
	// Stop while waiting for an approval cancels it (no run is left to report it): the next run starts with work.approval resolved (cancelled), once.
	// Arrange
	r := newApprovalRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "w1", "write_file", writeArgs)}},
		llmtest.Turn{Chunks: llmtest.Text("OK, something else")},
		llmtest.Turn{Chunks: llmtest.Text("Fine")},
	)
	th, first := r.interactive("report")
	r.process(first)
	approval := r.thread(th.ID).PendingApproval
	if _, err := r.st.CancelPendingApproval(context.Background(), first.ID, store.ApprovalCancelledResult); err != nil {
		t.Fatal(err)
	}
	second := r.create(th, store.NewRun{Message: store.Message{Role: store.RoleUser, Content: "never mind"}})

	// Act
	r.process(second)
	third := r.process(r.create(th, store.NewRun{Message: store.Message{Role: store.RoleUser, Content: "thanks"}}))

	// Assert
	resolved := approvalEvents(r.events(second.ID))
	if len(resolved) != 1 || resolved[0]["phase"] != "resolved" {
		t.Fatalf("approval events %v", resolved)
	}
	if a := resolved[0]["approval"].(map[string]any); a["id"] != approval.ID || a["status"] != "cancelled" {
		t.Fatalf("announced %v", a)
	}
	if again := approvalEvents(r.events(third.ID)); len(again) != 0 {
		t.Fatalf("announced twice: %v", again)
	}
}

// originalNamed is a tool whose exposed name hides its original one (truncated MCP name).
type originalNamed struct {
	tool.InvokableTool
	original string
}

func (o originalNamed) ApprovalNames() []string { return []string{o.original} }

func TestProcessGatesAToolByItsOriginalName(t *testing.T) {
	// A tool whose exposed name was truncated ("srv__de") but whose original name is risky ("srv__delete_everything") waits for approval.
	// Arrange
	r := newApprovalRig(t, llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "d1", "srv__de", `{}`)}})
	ran := false
	inv := scriptedTool(t, "srv__de", func(context.Context) (string, error) { ran = true; return "gone", nil }).(tool.InvokableTool)
	addTools(t, r, originalNamed{InvokableTool: inv, original: "srv__delete_everything"})
	th, run := r.interactive("clean up")

	// Act
	r.process(run)

	// Assert
	if a := r.thread(th.ID).PendingApproval; a == nil || a.ToolName != "srv__de" || ran {
		t.Fatalf("pending %+v ran %v", a, ran)
	}
}

// switchModeTool is a harmless tool that switches the thread's interaction mode while the run is active, like the composer toggle.
func switchModeTool(t *testing.T, r *rig, th *store.Thread, mode string) tool.BaseTool {
	t.Helper()
	return scriptedTool(t, "switch_mode", func(ctx context.Context) (string, error) {
		if err := r.st.SetThreadInteractionMode(ctx, "alice", th.ID, mode); err != nil {
			return "", err
		}
		return "switched", nil
	})
}

func TestProcessSwitchToInteractiveMidRunGatesTheNextRiskyCall(t *testing.T) {
	// A run started automatic asks for approval of the risky calls made after the user switched the thread to interactive.
	// Arrange
	r := newApprovalRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "s1", "switch_mode", `{}`)}},
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "w1", "write_file", writeArgs)}},
	)
	th, run := r.queued("write the report")
	addTools(t, r, switchModeTool(t, r, th, store.InteractionInteractive))

	// Act
	got := r.process(run)

	// Assert
	pending := r.thread(th.ID).PendingApproval
	if got.Status != store.StatusCompleted || pending == nil || pending.ToolCallID != "w1" || pending.Status != store.ApprovalPending {
		t.Fatalf("status %s pending %+v types %v", got.Status, pending, types(r.events(run.ID)))
	}
	if _, ok := r.file("/workspace/report.md"); ok {
		t.Fatal("the risky call ran after the switch to interactive")
	}
}

func TestProcessSwitchToAutomaticMidRunStopsAskingForApproval(t *testing.T) {
	// A run started interactive runs the risky calls made after the user switched the thread to automatic without asking.
	// Arrange
	r := newApprovalRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "s1", "switch_mode", `{}`)}},
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "w1", "write_file", writeArgs)}},
		llmtest.Turn{Chunks: llmtest.Text("Written")},
	)
	th, run := r.interactive("write the report")
	addTools(t, r, switchModeTool(t, r, th, store.InteractionAutomatic))

	// Act
	got := r.process(run)

	// Assert
	if got.Status != store.StatusCompleted || len(r.st.ApprovalsOf(th.ID)) != 0 || last(r.events(run.ID))["outcome"].(map[string]any)["type"] != "success" {
		t.Fatalf("status %s approvals %d types %v", got.Status, len(r.st.ApprovalsOf(th.ID)), types(r.events(run.ID)))
	}
	if content, ok := r.file("/workspace/report.md"); !ok || content != "# Report" {
		t.Fatalf("file %q %v", content, ok)
	}
}

func TestProcessSwitchWhileAnApprovalIsPendingKeepsTheCardAndResumesInTheNewMode(t *testing.T) {
	// Switching to automatic while a card waits does not decide it; once approved, the resume run runs its later risky calls without asking.
	// Arrange
	notesArgs := `{"path":"/workspace/notes.md","content":"notes"}`
	r := newApprovalRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "w1", "write_file", writeArgs)}},
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "w2", "write_file", notesArgs)}},
		llmtest.Turn{Chunks: llmtest.Text("Both written")},
	)
	th, first := r.interactive("write the report and the notes")
	r.process(first)
	if err := r.st.SetThreadInteractionMode(context.Background(), "alice", th.ID, store.InteractionAutomatic); err != nil {
		t.Fatal(err)
	}
	if pending := r.thread(th.ID).PendingApproval; pending == nil || pending.Status != store.ApprovalPending {
		t.Fatalf("the switch decided the card: %+v", pending)
	}
	second := r.decide(th, store.DecisionApprove, store.ApprovalResponse{})

	// Act
	got := r.process(second)

	// Assert
	if got.Status != store.StatusCompleted || got.InteractionMode != store.InteractionAutomatic || len(r.st.ApprovalsOf(th.ID)) != 1 {
		t.Fatalf("status %s mode %q approvals %d types %v", got.Status, got.InteractionMode, len(r.st.ApprovalsOf(th.ID)), types(r.events(second.ID)))
	}
	report, okReport := r.file("/workspace/report.md")
	notes, okNotes := r.file("/workspace/notes.md")
	if !okReport || report != "# Report" || !okNotes || notes != "notes" {
		t.Fatalf("report %q %v notes %q %v", report, okReport, notes, okNotes)
	}
}

func TestProcessSwitchToAutomaticMidStepKeepsLaterRiskyCallsBehindThePausedOne(t *testing.T) {
	// [write_file A, switch to automatic, write_file B]: A pauses, the switch runs, B waits behind A instead of running first; once A is approved the resume run takes A then B, in order, without asking again.
	// Arrange
	notesArgs := `{"path":"/workspace/notes.md","content":"notes"}`
	r := newApprovalRig(t,
		llmtest.Turn{Chunks: []*schema.Message{
			llmtest.ToolCall(0, "w1", "write_file", writeArgs),
			llmtest.ToolCall(1, "s1", "switch_mode", `{}`),
			llmtest.ToolCall(2, "w2", "write_file", notesArgs)}},
		llmtest.Turn{Chunks: llmtest.Text("Both written")},
	)
	th, first := r.interactive("write the report and the notes")
	addTools(t, r, switchModeTool(t, r, th, store.InteractionAutomatic))

	// Act
	r.process(first)
	firstResults := find(r.events(first.ID), "TOOL_CALL_RESULT")
	_, notesBeforeDecision := r.file("/workspace/notes.md")
	pending := r.thread(th.ID).PendingApproval
	second := r.process(r.decide(th, store.DecisionApprove, store.ApprovalResponse{}))

	// Assert
	if len(firstResults) != 1 || firstResults[0]["toolCallId"] != "s1" || notesBeforeDecision {
		t.Fatalf("first run: results %v, notes written before the paused call: %v", firstResults, notesBeforeDecision)
	}
	if pending == nil || pending.ToolCallID != "w1" || pending.Status != store.ApprovalPending {
		t.Fatalf("pending %+v", pending)
	}
	var order []any
	for _, e := range find(r.events(second.ID), "TOOL_CALL_RESULT") {
		order = append(order, e["toolCallId"])
	}
	if second.Status != store.StatusCompleted || len(order) != 2 || order[0] != "w1" || order[1] != "w2" || len(r.st.ApprovalsOf(th.ID)) != 1 {
		t.Fatalf("resume: status %s results %v approvals %d types %v", second.Status, order, len(r.st.ApprovalsOf(th.ID)), types(r.events(second.ID)))
	}
	if notes, ok := r.file("/workspace/notes.md"); !ok || notes != "notes" {
		t.Fatalf("notes %q %v", notes, ok)
	}
}
