package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/approvals"
	"github.com/7ar-chat/harness/engine/tools"
)

const rmArgs = `{"command":"rm -rf build"}`

// interactiveRig wires the real approval gate on a memory store, like the host does.
func interactiveRig(t *testing.T, turns ...llmtest.Turn) (*runRig, *funcTool) {
	t.Helper()
	r := newRunRig(t, turns...)
	r.a.Harness.BeforeTool = []BeforeToolHook{&approvals.Gate{Store: r.st}}
	r.a.Harness.Approvals = r.st
	exec := tool("execute_command", "removed")
	r.withTools(exec, tool("read_file", "contents"))
	return r, exec
}

func (r *runRig) interactiveRun(content string) *store.Run {
	return r.newRun(store.NewRun{InteractionMode: store.InteractionInteractive, Message: store.Message{Role: store.RoleUser, Content: content}})
}

// finish ends a run in the store so the thread can take the next one.
func (r *runRig) finish(run *store.Run) {
	r.t.Helper()
	ctx := context.Background()
	if _, err := r.st.ClaimRun(ctx, run.ID, "rt"); err != nil {
		r.t.Fatal(err)
	}
	if err := r.st.FinishRun(ctx, run.ID, "rt", store.StatusCompleted, "", ""); err != nil {
		r.t.Fatal(err)
	}
}

func (r *runRig) pending() *store.Approval {
	r.t.Helper()
	th, err := r.st.GetThread(context.Background(), "alice", r.th.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return th.PendingApproval
}

func (r *runRig) decide(decision string, resp store.ApprovalResponse) *store.Run {
	r.t.Helper()
	a := r.pending()
	if a == nil {
		r.t.Fatal("no pending approval")
	}
	return r.newRun(store.NewRun{Resume: &store.Resume{InterruptID: a.ID, Decision: decision, Response: resp}})
}

func TestRunPausesBeforeARiskyCallInInteractiveMode(t *testing.T) {
	// Arrange
	r, exec := interactiveRig(t, callTurn("c1", "execute_command", rmArgs))
	run := r.interactiveRun("clean up")

	// Act
	res, err := r.run(run)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(exec.calls()) != 0 {
		t.Fatal("the risky call ran before the user decided")
	}
	if res.Interrupt == nil || res.Interrupt.Reason != store.ReasonToolApproval || res.Interrupt.ToolCallID != "c1" || res.Interrupt.ID == "" || res.Interrupt.Approval == nil {
		t.Fatalf("interrupt %+v", res.Interrupt)
	}
	r.finish(run) // a thread waits for the approval of a completed run
	a := r.pending()
	if a == nil || a.ToolName != "execute_command" || a.ToolInputPreview != `{"command":"rm -rf build"}` || a.RunID != run.ID {
		t.Fatalf("pending approval %+v", a)
	}
	ev := r.events(agui.Custom + ":" + agui.CustomApproval)
	if len(ev) != 1 || ev[0]["value"].(map[string]any)["phase"] != "required" {
		t.Fatalf("approval events %v", ev)
	}
	if len(r.events(agui.ToolCallResult)) != 0 {
		t.Fatal("a paused call got a result")
	}
	if len(r.model.Calls()) != 1 {
		t.Fatal("the run went on after pausing")
	}
}

func TestRunRunsTheApprovedCallWithoutAskingTheModelAgain(t *testing.T) {
	// Arrange
	r, exec := interactiveRig(t, callTurn("c1", "execute_command", rmArgs), textTurn("build removed"))
	first := r.interactiveRun("clean up")
	if _, err := r.run(first); err != nil {
		t.Fatal(err)
	}
	r.finish(first)
	second := r.decide(store.DecisionApprove, store.ApprovalResponse{})

	// Act
	res, err := r.run(second)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got := exec.calls(); len(got) != 1 || got[0] != rmArgs {
		t.Fatalf("tool calls %v", got)
	}
	if res.Interrupt != nil {
		t.Fatalf("interrupted again: %+v", res.Interrupt)
	}
	ev := r.events(agui.Custom + ":" + agui.CustomApproval)
	if len(ev) != 1 || ev[0]["value"].(map[string]any)["phase"] != "resolved" {
		t.Fatalf("approval events %v", ev)
	}
	msgs := r.model.Calls()[1].Messages
	var sawResult bool
	for _, m := range msgs {
		sawResult = sawResult || (m.Role == ai.RoleTool && m.ToolCallID == "c1" && m.Content == "removed")
	}
	if !sawResult {
		t.Fatalf("the model did not see the approved call's result: %+v", msgs)
	}
	// The result fills the call's own slot: no duplicate tool message.
	tools := 0
	for _, m := range r.messages() {
		if m.Role == store.RoleTool && m.ToolCallID == "c1" {
			tools++
		}
	}
	if tools != 1 {
		t.Fatalf("%d stored results for c1", tools)
	}
}

func TestRunDoesNotRunADeniedCallAndTellsTheModel(t *testing.T) {
	for name, tc := range map[string]struct {
		decision string
		resp     store.ApprovalResponse
		want     string
	}{
		"deny":    {store.DecisionDeny, store.ApprovalResponse{}, store.ApprovalDeniedResult},
		"respond": {store.DecisionRespond, store.ApprovalResponse{ResponseText: "use make clean"}, "free text: use make clean"},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			r, exec := interactiveRig(t, callTurn("c1", "execute_command", rmArgs), textTurn("ok, using make clean"))
			first := r.interactiveRun("clean up")
			if _, err := r.run(first); err != nil {
				t.Fatal(err)
			}
			r.finish(first)

			// Act
			_, err := r.run(r.decide(tc.decision, tc.resp))

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			if len(exec.calls()) != 0 {
				t.Fatal("a refused call ran")
			}
			msgs := r.model.Calls()[1].Messages
			var content string
			for _, m := range msgs {
				if m.Role == ai.RoleTool && m.ToolCallID == "c1" {
					content = m.Content
				}
			}
			if !strings.Contains(content, tc.want) {
				t.Fatalf("model saw %q, want %q", content, tc.want)
			}
		})
	}
}

// changedArgs makes the resumed approval describe other arguments than the stored call.
type changedArgs struct{ src ApprovalSource }

func (c changedArgs) ResumedApproval(ctx context.Context, runID string) (*store.Approval, error) {
	a, err := c.src.ResumedApproval(ctx, runID)
	if a != nil {
		a.ToolInputPreview = `{"command":"ls"}`
	}
	return a, err
}

func TestRunRefusesAnApprovedCallWhoseArgumentsNoLongerMatch(t *testing.T) {
	// Arrange
	r, exec := interactiveRig(t, callTurn("c1", "execute_command", rmArgs), textTurn("understood"))
	first := r.interactiveRun("clean up")
	if _, err := r.run(first); err != nil {
		t.Fatal(err)
	}
	r.finish(first)
	second := r.decide(store.DecisionApprove, store.ApprovalResponse{})
	r.a.Harness.Approvals = changedArgs{r.st}

	// Act
	_, err := r.run(second)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(exec.calls()) != 0 {
		t.Fatal("a call that does not match its approval ran")
	}
	res := r.events(agui.ToolCallResult)
	if len(res) != 1 || res[0]["content"] != approvals.MismatchResult {
		t.Fatalf("results %v", res)
	}
}

func TestRunNeverAsksToApproveACallTheLoopGuardWouldRefuse(t *testing.T) {
	// Arrange: the same call runs twice unasked; from then on the hook would gate it.
	same := func(id string) llmtest.Turn { return callTurn(id, "fetch", `{"p":1}`) }
	r := newRunRig(t, same("c1"), same("c2"), same("c3"), textTurn("done"))
	runs := &atomic.Int32{}
	r.withTools(&funcTool{name: "fetch", run: func(context.Context, string) (string, error) { runs.Add(1); return "r", nil }})
	r.a.Harness.BeforeTool = []BeforeToolHook{gateAfter{runs: runs, n: 2}}

	// Act
	res, err := r.do("go")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if res.Interrupt != nil {
		t.Fatal("the user was asked to approve a call that is refused anyway")
	}
	got := r.events(agui.ToolCallResult)
	if len(got) != 3 || got[2]["content"] != fmt.Sprintf(LoopGuardResult, "fetch") || runs.Load() != 2 {
		t.Fatalf("results %v, runs %d", got, runs.Load())
	}
}

// gateAfter gates every call once the tool ran n times.
type gateAfter struct {
	alwaysAsk
	runs *atomic.Int32
	n    int32
}

func (g gateAfter) NeedsApproval(context.Context, *store.Run, string, string) bool {
	return g.runs.Load() >= g.n
}

// alwaysAsk gates every call.
type alwaysAsk struct{ pauseErr error }

func (a alwaysAsk) NeedsApproval(context.Context, *store.Run, string, string) bool { return true }
func (a alwaysAsk) Pause(_ context.Context, run *store.Run, tc ai.ToolCall) (*tools.Interrupt, error) {
	if a.pauseErr != nil {
		return nil, a.pauseErr
	}
	return &tools.Interrupt{Reason: store.ReasonToolApproval, Approval: &store.Approval{ID: "ap-1", ThreadID: run.ThreadID, RunID: run.ID, ToolCallID: tc.ID, ToolName: tc.Function.Name}}, nil
}

func TestRunRefusesACallWhoseApprovalCannotBeRecorded(t *testing.T) {
	for name, hook := range map[string]BeforeToolHook{
		"store error": alwaysAsk{pauseErr: errors.New("db down")},
		"panic":       panicPause{},
		"no approval": noApproval{},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			r, exec := interactiveRig(t, callTurn("c1", "read_file", `{}`), textTurn("could not"))
			r.a.Harness.BeforeTool = []BeforeToolHook{hook}

			// Act
			res, err := r.do("go")

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			_ = exec
			if res.Interrupt != nil {
				t.Fatal("paused although nothing was recorded")
			}
			got := r.events(agui.ToolCallResult)
			if len(got) != 1 || got[0]["content"] != approvals.PauseFailedResult {
				t.Fatalf("results %v", got)
			}
		})
	}
}

type panicPause struct{ alwaysAsk }

func (panicPause) Pause(context.Context, *store.Run, ai.ToolCall) (*tools.Interrupt, error) {
	panic("pause bug")
}

type noApproval struct{ alwaysAsk }

func (noApproval) Pause(context.Context, *store.Run, ai.ToolCall) (*tools.Interrupt, error) {
	return &tools.Interrupt{Reason: store.ReasonToolApproval}, nil
}

func TestRunTakesTheInterruptIDFromTheApprovalWhenTheHookLeavesItEmpty(t *testing.T) {
	// Arrange
	r, _ := interactiveRig(t, callTurn("c1", "read_file", `{}`))
	r.a.Harness.BeforeTool = []BeforeToolHook{alwaysAsk{}}

	// Act
	res, err := r.do("go")

	// Assert
	if err != nil || res.Interrupt == nil || res.Interrupt.ID != "ap-1" || res.Interrupt.ToolCallID != "c1" {
		t.Fatalf("res %+v err %v", res.Interrupt, err)
	}
}

type denier struct {
	alwaysAsk
	text   string
	panics bool
}

func (d denier) NeedsApproval(context.Context, *store.Run, string, string) bool { return false }
func (d denier) Denial(_ context.Context, _ *store.Run, tool, _ string) string {
	if d.panics {
		panic("policy bug")
	}
	if tool == "execute_command" {
		return d.text
	}
	return ""
}

func TestRunRefusesACallADenierRefusesWhateverTheMode(t *testing.T) {
	for name, tc := range map[string]struct {
		d    denier
		want string
	}{
		"denied":         {denier{text: "Error: not allowed here"}, "Error: not allowed here"},
		"policy crashed": {denier{panics: true}, "Error: the permission check crashed, so the action was not run."},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			r, exec := interactiveRig(t, callTurn("c1", "execute_command", rmArgs), textTurn("ok"))
			r.a.Harness.BeforeTool = []BeforeToolHook{tc.d}

			// Act
			_, err := r.do("go")

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			if len(exec.calls()) != 0 {
				t.Fatal("denied call ran")
			}
			res := r.events(agui.ToolCallResult)
			if len(res) != 1 || res[0]["content"] != tc.want || res[0]["metadata"].(map[string]any)["isError"] != true {
				t.Fatalf("results %v", res)
			}
			if tm := r.lastMessage(store.RoleTool); tm.Error != tc.want {
				t.Fatalf("stored error %q", tm.Error)
			}
		})
	}
}

func TestRunDoesNotGateUnknownToolsOrBrokenArguments(t *testing.T) {
	// Arrange: a hook that gates everything must not pause a call that cannot run anyway.
	r, _ := interactiveRig(t, llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "c1", "nope", `{}`), llmtest.ToolCall(1, "c2", "read_file", `{"p":`)}}, textTurn("ok"))
	r.a.Harness.BeforeTool = []BeforeToolHook{alwaysAsk{}, denier{text: "x"}}

	// Act
	res, err := r.do("go")

	// Assert
	if err != nil || res.Interrupt != nil {
		t.Fatalf("res %+v err %v", res.Interrupt, err)
	}
	if got := r.events(agui.ToolCallResult); len(got) != 2 || !strings.Contains(fmt.Sprint(got[0]["content"]), "unknown tool") || !strings.Contains(fmt.Sprint(got[1]["content"]), "not valid JSON") {
		t.Fatalf("results %v", got)
	}
}

// riskOnly implements RiskJudge: the live mode says no, the judge says yes.
type riskOnly struct {
	alwaysAsk
	gated bool
}

func (riskOnly) Risky(context.Context, *store.Run, string, string) bool { return true }
func (r riskOnly) NeedsApproval(_ context.Context, _ *store.Run, tool string, _ string) bool {
	return r.gated && tool == "execute_command"
}

func TestRunLeavesLaterRiskyCallsBehindAPendingApprovalEvenIfTheModeSwitchedToAutomatic(t *testing.T) {
	// Arrange: the first call needs approval; the second is judged risky but, in the live mode, would not be gated.
	r, exec := interactiveRig(t, llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "c1", "execute_command", rmArgs), llmtest.ToolCall(1, "c2", "write_file", `{"path":"/etc/x"}`)}})
	r.withTools(exec, tool("write_file", "written"))
	r.a.Harness.BeforeTool = []BeforeToolHook{riskOnly{gated: true}}

	// Act
	res, err := r.do("go")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if res.Interrupt == nil || res.Interrupt.ToolCallID != "c1" {
		t.Fatalf("interrupt %+v", res.Interrupt)
	}
	if got := r.events(agui.ToolCallResult); len(got) != 0 {
		t.Fatalf("the deferred call got a result: %v", got)
	}
}
