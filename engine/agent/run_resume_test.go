package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/tools"
)

type turnObserver struct {
	funcTool
	got []tools.TurnCall
}

func (o *turnObserver) ObserveTurn(calls []tools.TurnCall) { o.got = calls }

func askTool() *funcTool {
	return &funcTool{name: "ask_user", run: func(_ context.Context, args string) (string, error) {
		return "", &tools.Interrupt{Reason: "ask_user", Question: "Which format?"}
	}}
}

// askAndAnswer runs a first run that asks the user a question and returns the resume run carrying the answer.
func askAndAnswer(t *testing.T, r *runRig, answer string) *store.Run {
	t.Helper()
	first := r.userRun("make the report")
	res, err := r.run(first)
	if err != nil || res.Interrupt == nil {
		t.Fatalf("first run: %+v %v", res.Interrupt, err)
	}
	if err := r.st.SetPendingQuestion(context.Background(), r.th.ID, &store.PendingQuestion{InterruptID: res.Interrupt.ID, RunID: first.ID, ToolCallID: res.Interrupt.ToolCallID}); err != nil {
		t.Fatal(err)
	}
	r.finish(first)
	return r.newRun(store.NewRun{Resume: &store.Resume{InterruptID: res.Interrupt.ID, Content: answer}})
}

func TestRunResumedAfterAnAnswerReplaysItAndTheModelSeesItAsTheToolResult(t *testing.T) {
	// Arrange
	r := newRunRig(t, callTurn("q1", "ask_user", `{"question":"Which format?"}`), textTurn("Here is the PDF"))
	r.withTools(askTool())
	resume := askAndAnswer(t, r, "PDF please")

	// Act
	res, err := r.run(resume)

	// Assert
	if err != nil || res.Interrupt != nil {
		t.Fatalf("res %+v err %v", res.Interrupt, err)
	}
	replayed := r.events(agui.ToolCallResult)
	if len(replayed) != 1 || replayed[0]["content"] != "PDF please" || replayed[0]["toolCallId"] != "q1" {
		t.Fatalf("replayed results %v", replayed)
	}
	if r.types()[0] != "TOOL_CALL_RESULT" {
		t.Fatalf("the replayed answer does not come first: %v", r.types())
	}
	msgs := r.model.Calls()[1].Messages
	var found bool
	for _, m := range msgs {
		found = found || (m.Role == ai.RoleTool && m.ToolCallID == "q1" && m.Content == "PDF please")
	}
	if !found {
		t.Fatalf("model input %+v", msgs)
	}
}

func TestRunRunsTheSiblingCallsOfAQuestionInTheSameStep(t *testing.T) {
	// Arrange: the model asks a question and calls another tool in the same step: only the question waits for the user.
	r := newRunRig(t, llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "q1", "ask_user", `{}`), llmtest.ToolCall(1, "w1", "write", `{"n":1}`)}}, textTurn("all done"))
	writer := tool("write", "written")
	r.withTools(askTool(), writer)

	// Act
	res, err := r.do("go")

	// Assert
	if err != nil || res.Interrupt == nil || res.Interrupt.ToolCallID != "q1" {
		t.Fatalf("%+v %v", res.Interrupt, err)
	}
	if got := writer.calls(); len(got) != 1 {
		t.Fatalf("sibling call ran %d times in the first run", len(got))
	}
	if got := r.events(agui.ToolCallResult); len(got) != 1 || got[0]["toolCallId"] != "w1" {
		t.Fatalf("first run results %v", got)
	}
}

func TestRunGivesTheTurnsEarlierCallsToToolsThatKeepStateAcrossRuns(t *testing.T) {
	// Arrange
	r := newRunRig(t, callTurn("c1", "t", `{"a":1}`), callTurn("c2", "t", `{"a":2}`), textTurn("done"))
	obs := &turnObserver{funcTool: *tool("t", "r")}
	obs.funcTool.run = func(context.Context, string) (string, error) { return "", fmt.Errorf("Error: nope") }
	r.withTools(obs)
	first := r.userRun("go")
	r.a.MaxSteps = 2
	if _, err := r.run(first); err == nil {
		t.Fatal("expected the step limit")
	}
	r.finish(first)
	r.a.MaxSteps = 6

	// Act: a run that continues the same turn (a steer-less resume with no new message).
	next := r.newRun(store.NewRun{})
	if _, err := r.run(next); err != nil {
		t.Fatal(err)
	}

	// Assert
	if len(obs.got) != 1 || obs.got[0].Name != "t" || obs.got[0].Arguments != `{"a":1}` || !obs.got[0].Failed || !strings.Contains(obs.got[0].Result, "nope") {
		t.Fatalf("observed turn %+v", obs.got)
	}
}

func TestRunRestoresTheLoopGuardsFromTheTurnsEarlierCalls(t *testing.T) {
	// Arrange: two identical calls ran in an earlier run of the turn; the third in this run is refused.
	r := newRunRig(t, callTurn("c1", "t", `{"a":1}`), callTurn("c2", "t", `{"a":1}`), callTurn("c3", "t", `{"a":1}`), textTurn("stop"))
	tl := tool("t", "r")
	r.withTools(tl)
	first := r.userRun("go")
	r.a.MaxSteps = 2
	_, _ = r.run(first)
	r.finish(first)
	r.a.MaxSteps = 6

	// Act
	_, err := r.run(r.newRun(store.NewRun{}))

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if n := len(tl.calls()); n != 2 {
		t.Fatalf("tool ran %d times; the third identical call must be refused", n)
	}
}

func TestRunMarksTheContextUntrustedOnceAToolBroughtOutsideContent(t *testing.T) {
	for name, tc := range map[string]struct {
		tool string
		want bool
	}{
		"web fetch":     {"web_fetch", true},
		"update_plan":   {"update_plan", false},
		"memory search": {"memory_search", false},
		"write_file":    {"write_file", false},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			r := newRunRig(t, callTurn("c1", tc.tool, `{}`), textTurn("ok"))
			var seen bool
			r.withTools(&funcTool{name: tc.tool}, probeUntrusted(&seen))
			r.model = llmtest.New(callTurn("c1", tc.tool, `{}`), callTurn("c2", "probe", `{"x":1}`), textTurn("ok"))
			r.a.Models = llmtest.Factory{M: r.model}

			// Act
			if _, err := r.do("go"); err != nil {
				t.Fatal(err)
			}

			// Assert
			if seen != tc.want {
				t.Fatalf("untrusted = %v", seen)
			}
		})
	}
}

func probeUntrusted(seen *bool) ai.BaseTool {
	return &funcTool{name: "probe", run: func(ctx context.Context, _ string) (string, error) {
		*seen = tools.Untrusted(ctx)
		return "r", nil
	}}
}

func TestRunStartsUntrustedWhenTheHistoryAlreadyHoldsOutsideContent(t *testing.T) {
	// Arrange
	r := newRunRig(t, textTurn("ok"))
	first := r.userRun("fetch it")
	_ = r.st.AppendMessages(context.Background(),
		&store.Message{ThreadID: r.th.ID, RunID: first.ID, ID: "a1", Role: store.RoleAssistant, ToolCalls: []store.ToolCall{{ID: "c1", Type: "function", Function: store.ToolCallFunction{Name: "web_fetch", Arguments: "{}"}}}},
		&store.Message{ThreadID: r.th.ID, RunID: first.ID, ID: "t1", Role: store.RoleTool, ToolCallID: "c1", Content: "page text"})
	r.finish(first)
	var seen bool
	r.a.Tools = &provider{tools: []ai.BaseTool{probeUntrusted(&seen)}}
	r.model = llmtest.New(callTurn("p", "probe", `{}`), textTurn("ok"))
	r.a.Models = llmtest.Factory{M: r.model}

	// Act
	if _, err := r.do("and now?"); err != nil {
		t.Fatal(err)
	}

	// Assert
	if !seen {
		t.Fatal("a run that follows fetched content started trusted")
	}
}

func TestUntrustedResultTreatsOnlyUserSkillsOfLoadSkillAsUntrusted(t *testing.T) {
	if untrustedResult("load_skill", "built-in instructions") {
		t.Error("a built-in skill is untrusted")
	}
	if !untrustedResult("load_skill", `<skill source="user" name="x">…`) {
		t.Error("a user skill is trusted")
	}
	if untrustedResult("ask_user", "anything") || !untrustedResult("execute_command", "anything") {
		t.Error("trusted tool table wrong")
	}
}
