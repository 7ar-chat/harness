package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/tools"
)

func TestRunAnswersAndStoresTheConversation(t *testing.T) {
	// Arrange
	r := newRunRig(t, llmtest.Turn{Chunks: []*ai.Message{{Content: "Hel"}, llmtest.WithUsage(&ai.Message{Content: "lo"}, 120, 30)}})

	// Act
	res, err := r.do("hi")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	want := "STEP_STARTED TEXT_MESSAGE_START TEXT_MESSAGE_CONTENT TEXT_MESSAGE_CONTENT TEXT_MESSAGE_END CUSTOM:work.usage STEP_FINISHED"
	if got := strings.Join(r.types(), " "); got != want {
		t.Fatalf("events:\n got %s\nwant %s", got, want)
	}
	if a := r.lastMessage(store.RoleAssistant); a.Content != "Hello" || a.RunID == "" {
		t.Fatalf("assistant %+v", a)
	}
	if len(res.Usage) != 1 || res.Usage[0].InputTokens != 120 || res.Usage[0].OutputTokens != 30 || res.Usage[0].Model != "gpt-test" || res.Interrupt != nil {
		t.Fatalf("usage %+v", res.Usage)
	}
	if r.w.flushes == 0 {
		t.Error("the step's events were not flushed")
	}
}

func TestRunRunsToolsAndFeedsTheirResultsBack(t *testing.T) {
	// Arrange
	r := newRunRig(t, callTurn("c1", "lookup", `{"q":"x"}`), textTurn("found it"))
	lookup := tool("lookup", "42")
	cleaner := r.withTools(lookup)

	// Act
	_, err := r.do("find x")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got := lookup.calls(); len(got) != 1 || got[0] != `{"q":"x"}` {
		t.Fatalf("tool calls %v", got)
	}
	if cleaner.cleaned != 1 {
		t.Errorf("tool cleanup ran %d times", cleaner.cleaned)
	}
	second := r.model.Calls()[1].Messages
	lastTool := second[len(second)-1]
	if lastTool.Role != ai.RoleTool || lastTool.Content != "42" || lastTool.ToolCallID != "c1" || lastTool.ToolName != "lookup" {
		t.Fatalf("model did not see the result: %+v", lastTool)
	}
	if got := strings.Join(r.roles(), ","); got != "user,assistant,tool,assistant" {
		t.Fatalf("stored roles %s", got)
	}
	results := r.events(agui.ToolCallResult)
	if len(results) != 1 || results[0]["content"] != "42" || results[0]["toolCallId"] != "c1" {
		t.Fatalf("result events %v", results)
	}
	if len(r.model.Calls()[0].Tools) != 1 {
		t.Error("the model was not offered the tool")
	}
}

func TestRunGivesTheModelAnErrorResultForBadCalls(t *testing.T) {
	for name, tc := range map[string]struct {
		call    ai.ToolCall
		contain string
	}{
		"unknown tool":    {ai.ToolCall{ID: "c1", Function: ai.FunctionCall{Name: "nope", Arguments: "{}"}}, `unknown tool "nope"; available tools: lookup`},
		"invalid json":    {ai.ToolCall{ID: "c1", Function: ai.FunctionCall{Name: "lookup", Arguments: `{"q":`}}, "arguments of lookup are not valid JSON"},
		"tool error":      {ai.ToolCall{ID: "c1", Function: ai.FunctionCall{Name: "lookup", Arguments: `{"fail":true}`}}, "Error: backend down"},
		"user error":      {ai.ToolCall{ID: "c1", Function: ai.FunctionCall{Name: "lookup", Arguments: `{"user":true}`}}, "Error: path is outside the workspace"},
		"empty arguments": {ai.ToolCall{ID: "c1", Function: ai.FunctionCall{Name: "lookup"}}, "args={}"},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			tc.call.Type = "function"
			r := newRunRig(t, llmtest.Turn{Chunks: []*ai.Message{{ToolCalls: []ai.ToolCall{tc.call}}}}, textTurn("sorry"))
			r.withTools(&funcTool{name: "lookup", run: func(_ context.Context, args string) (string, error) {
				switch {
				case strings.Contains(args, "fail"):
					return "", errors.New("backend down")
				case strings.Contains(args, "user"):
					return "", errors.New("ignored")
				}
				return "args=" + args, nil
			}})
			if name == "user error" {
				r.withTools(&funcTool{name: "lookup", run: func(context.Context, string) (string, error) {
					return "", userErr("path is outside the workspace")
				}})
			}

			// Act
			_, err := r.do("go")

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			res := r.events(agui.ToolCallResult)
			if len(res) != 1 || !strings.Contains(res[0]["content"].(string), tc.contain) {
				t.Fatalf("result %v, want %q", res, tc.contain)
			}
			isErr := name != "empty arguments"
			if md, _ := res[0]["metadata"].(map[string]any); (md["isError"] == true) != isErr {
				t.Fatalf("metadata %v", md)
			}
			if tm := r.lastMessage(store.RoleTool); (tm.Error != "") != isErr {
				t.Fatalf("stored error %q", tm.Error)
			}
		})
	}
}

func userErr(msg string) error { return fmt.Errorf("wrapped: %w", tools.Errorf("%s", msg)) }

func TestRunCapsThePlainToolOutput(t *testing.T) {
	// Arrange
	r := newRunRig(t, callTurn("c1", "dump", "{}"), textTurn("ok"))
	r.a.MaxToolOutputChars = 1000
	r.withTools(tool("dump", strings.Repeat("x", 5000)))

	// Act
	_, err := r.do("go")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got := r.events(agui.ToolCallResult)[0]["content"].(string); len(got) > 1100 || !strings.Contains(got, "not shown") && !strings.Contains(got, "truncated") && !strings.Contains(got, "…") {
		t.Fatalf("output of %d chars not capped: …%s", len(got), got[len(got)-60:])
	}
}

func TestRunStopsAtTheStepLimitWithAForcedAnswer(t *testing.T) {
	// Arrange: the model keeps calling a tool; on the last step it may no longer call tools.
	r := newRunRig(t, callTurn("c1", "t", `{"n":1}`), callTurn("c2", "t", `{"n":2}`), textTurn("here is what I have"))
	r.a.MaxSteps = 3
	r.withTools(tool("t", "r"))

	// Act
	_, err := r.do("go")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	calls := r.model.Calls()
	if len(calls) != 3 {
		t.Fatalf("%d model calls", len(calls))
	}
	last := calls[2]
	if last.ToolChoice == nil || *last.ToolChoice != ai.ToolChoiceForbidden || last.Messages[len(last.Messages)-1].Content != StepLimitNudge {
		t.Fatalf("last call: choice %v, nudge %q", last.ToolChoice, last.Messages[len(last.Messages)-1].Content)
	}
	if calls[0].ToolChoice != nil || calls[1].ToolChoice != nil {
		t.Error("tools were forbidden before the last step")
	}
	if len(last.Tools) != 1 {
		t.Error("the last step lost the tool definitions its history refers to")
	}
}

func TestRunFailsWithMaxStepsWhenTheLastStepStillCallsTools(t *testing.T) {
	// Arrange
	r := newRunRig(t, callTurn("c1", "t", `{"n":1}`), callTurn("c2", "t", `{"n":2}`))
	r.a.MaxSteps = 2
	tl := tool("t", "r")
	r.withTools(tl)

	// Act
	_, err := r.do("go")

	// Assert
	if errCode(t, err) != agui.CodeMaxSteps {
		t.Fatalf("err %v", err)
	}
	if n := len(tl.calls()); n != 1 {
		t.Fatalf("the tool ran %d times; the last step's call must not run", n)
	}
}

func TestRunContinuesAnAnswerCutByTheOutputLimitAtMostTwice(t *testing.T) {
	// Arrange
	cut := func(s string) llmtest.Turn {
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(&ai.Message{Content: s}, "length")}}
	}
	r := newRunRig(t, cut("part one "), cut("part two "), cut("part three "), textTurn("never asked"))

	// Act
	_, err := r.do("write a lot")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	calls := r.model.Calls()
	if len(calls) != 3 {
		t.Fatalf("%d model calls, want the first + 2 continuations", len(calls))
	}
	for i, c := range calls[1:] {
		if m := c.Messages[len(c.Messages)-1]; m.Content != ContinueAfterLengthNote {
			t.Errorf("call %d ends with %q", i+1, m.Content)
		}
	}
}

func TestRunNeverRunsToolCallsCutByTheOutputLimit(t *testing.T) {
	// Arrange
	r := newRunRig(t, llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", "t", `{"content":"half a fi`), "length")}}, textTurn("ok smaller"))
	tl := tool("t", "r")
	r.withTools(tl)

	// Act
	_, err := r.do("write")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.calls()) != 0 {
		t.Fatal("a truncated call was executed")
	}
	if res := r.events(agui.ToolCallResult); len(res) != 1 || res[0]["content"] != OutputLimitResult {
		t.Fatalf("results %v", res)
	}
	second := r.model.Calls()[1].Messages
	if m := second[len(second)-1]; m.Role != ai.RoleTool || m.Content != OutputLimitResult {
		t.Fatalf("model saw %+v", m)
	}
}

func TestRunFailsWhenTheProviderFiltersTheAnswer(t *testing.T) {
	// Arrange
	r := newRunRig(t, llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(&ai.Message{Content: "I can't"}, "content_filter")}})

	// Act
	_, err := r.do("go")

	// Assert
	if errCode(t, err) != agui.CodeContentFilter {
		t.Fatalf("err %v", err)
	}
	for _, m := range r.messages() {
		if m.Role == store.RoleAssistant {
			t.Fatalf("a filtered answer was stored: %q", m.Content)
		}
	}
	if len(r.events(agui.Custom+":"+agui.CustomUsage)) == 0 {
		t.Error("usage of the filtered call was not reported")
	}
}

func TestRunNudgesAnEmptyAnswerOnceThenFails(t *testing.T) {
	// Arrange
	r := newRunRig(t, llmtest.Turn{Chunks: []*ai.Message{{Content: ""}}}, llmtest.Turn{Chunks: []*ai.Message{{Content: "  "}}}, textTurn("never"))

	// Act
	_, err := r.do("go")

	// Assert
	if errCode(t, err) != agui.CodeEmptyAnswer {
		t.Fatalf("err %v", err)
	}
	calls := r.model.Calls()
	if len(calls) != 2 || calls[1].Messages[len(calls[1].Messages)-1].Content != EmptyAnswerNudge {
		t.Fatalf("calls %d", len(calls))
	}
}

func TestRunRecoversFromAnEmptyAnswerAfterTheNudge(t *testing.T) {
	// Arrange
	r := newRunRig(t, llmtest.Turn{Chunks: []*ai.Message{{Content: ""}}}, textTurn("sorry, here is the answer"))

	// Act
	_, err := r.do("go")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if a := r.lastMessage(store.RoleAssistant); a.Content != "sorry, here is the answer" {
		t.Fatalf("answer %q", a.Content)
	}
}

func TestRunAnEmptyAnswerNamesTheRefusalInTheDetailOnly(t *testing.T) {
	// Arrange
	r := newRunRig(t, llmtest.Turn{Refusal: "I cannot help with that", Chunks: []*ai.Message{{Content: ""}}}, llmtest.Turn{Refusal: "still no", Chunks: []*ai.Message{{Content: ""}}})

	// Act
	_, err := r.do("go")

	// Assert
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != agui.CodeEmptyAnswer || ae.Message != MsgEmptyAnswer || !strings.Contains(ae.Detail, "refusal: still no") {
		t.Fatalf("err %#v", err)
	}
}
