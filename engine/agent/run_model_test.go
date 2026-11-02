package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
)

type statusErr struct {
	code int
	msg  string
}

func (e statusErr) Error() string   { return e.msg }
func (e statusErr) StatusCode() int { return e.code }

func TestRunRetriesATransientModelFailureAndAnnouncesTheRetry(t *testing.T) {
	// Arrange
	r := newRunRig(t, llmtest.Turn{Err: statusErr{503, "status code: 503"}}, textTurn("recovered"))
	var slept []time.Duration
	r.a.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	r.a.RetryBackoff = 40 * time.Millisecond

	// Act
	_, err := r.do("go")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	retries := r.events(agui.Custom + ":" + agui.CustomRetry)
	if len(retries) != 1 {
		t.Fatalf("retry events %v", retries)
	}
	if v := retries[0]["value"].(map[string]any); v["attempt"] != 2 || v["reason"] != "transient" {
		t.Fatalf("retry value %v", v)
	}
	if len(slept) != 1 || slept[0] < 20*time.Millisecond || slept[0] > 60*time.Millisecond {
		t.Fatalf("slept %v", slept)
	}
	if a := r.lastMessage(store.RoleAssistant); a.Content != "recovered" {
		t.Fatalf("answer %q", a.Content)
	}
}

func TestRunClosesTheToolCallsOfAStreamThatBrokeBeforeRetrying(t *testing.T) {
	// Arrange: the first attempt announced a call and then the connection dropped.
	r := newRunRig(t,
		llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "c1", "t", `{"a":`)}, FailAfter: statusErr{502, "status code: 502"}},
		textTurn("second attempt"))
	r.withTools(tool("t", "r"))

	// Act
	_, err := r.do("go")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	res := r.events(agui.ToolCallResult)
	if len(res) != 1 || res[0]["content"] != StreamRetriedResult || res[0]["metadata"].(map[string]any)["retried"] != true {
		t.Fatalf("results %v", res)
	}
	for _, m := range r.messages() {
		if m.Role == store.RoleTool {
			t.Fatalf("the abandoned attempt's call was stored: %+v", m)
		}
	}
}

func TestRunMapsExhaustedRetriesToUserFacingErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code string
		msg  string
		n    int
	}{
		"rate limited":    {statusErr{429, "status code: 429"}, agui.CodeRateLimited, MsgRateLimited, 4},
		"server errors":   {statusErr{500, "status code: 500"}, agui.CodeModelError, MsgModelError, 4},
		"fatal, no retry": {statusErr{401, "status code: 401 bad key sk-secret"}, agui.CodeModelError, MsgModelError, 1},
		"timeout":         {context.DeadlineExceeded, agui.CodeModelTimeout, MsgModelTimeout, 4},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			turns := make([]llmtest.Turn, 6)
			for i := range turns {
				turns[i] = llmtest.Turn{Err: tc.err}
			}
			r := newRunRig(t, turns...)

			// Act
			_, err := r.do("go")

			// Assert
			var ae *Error
			if !errors.As(err, &ae) || ae.Code != tc.code || ae.Message != tc.msg {
				t.Fatalf("err %#v", err)
			}
			if strings.Contains(ae.Message, "sk-secret") {
				t.Error("provider text leaked to the client message")
			}
			if got := len(r.model.Calls()); got != tc.n {
				t.Errorf("%d model calls, want %d", got, tc.n)
			}
		})
	}
}

func TestRunHonoursStepRetriesSettings(t *testing.T) {
	for name, tc := range map[string]struct {
		setting int
		calls   int
	}{"never": {StepRetriesSetting(0), 1}, "once": {1, 2}} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			r := newRunRig(t, llmtest.Turn{Err: statusErr{503, "status code: 503"}}, llmtest.Turn{Err: statusErr{503, "status code: 503"}}, textTurn("late"))
			r.a.StepRetries = tc.setting

			// Act
			_, err := r.do("go")

			// Assert
			if err == nil || len(r.model.Calls()) != tc.calls {
				t.Fatalf("err %v calls %d", err, len(r.model.Calls()))
			}
		})
	}
}

func TestRunWithACancelledContextReturnsItsError(t *testing.T) {
	// Arrange
	r := newRunRig(t, llmtest.Turn{Chunks: llmtest.Text("partial answer"), Block: true})
	ctx, cancel := context.WithCancel(context.Background())
	run := r.userRun("go")
	go func() {
		for len(r.model.Calls()) == 0 {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	// Act
	_, err := r.a.Run(ctx, Input{Run: run}, r.w)

	// Assert
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if a := r.lastMessage(store.RoleAssistant); a.Content != "partial answer" {
		t.Fatalf("partial answer %q", a.Content)
	}
	if got := r.types(); got[len(got)-1] != "STEP_FINISHED" {
		t.Fatalf("the step was not closed: %v", got)
	}
}

func TestRunReportsEventLogFailuresAsInternalErrors(t *testing.T) {
	// Arrange
	r := newRunRig(t, textTurn("hi"))
	run := r.userRun("go")
	w := &recordingWriter{failOn: agui.StepStarted}

	// Act
	_, err := r.a.Run(context.Background(), Input{Run: run}, w)

	// Assert
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != agui.CodeInternal || ae.Message != MsgInternal || !strings.Contains(ae.Detail, "event log down") {
		t.Fatalf("err %#v", err)
	}
}

func TestRunFailsWhenThereIsNoModel(t *testing.T) {
	// Arrange
	r := newRunRig(t)
	r.a.Models = failingFactory{}

	// Act
	_, err := r.do("go")

	// Assert
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != agui.CodeModelError || !strings.Contains(ae.Detail, "no such model") {
		t.Fatalf("err %#v", err)
	}
}

type failingFactory struct{}

func (failingFactory) Model(context.Context, string) (ai.Model, error) {
	return nil, errors.New("no such model")
}

func TestRunGoesOnWithoutToolsWhenTheToolProviderFails(t *testing.T) {
	// Arrange
	r := newRunRig(t, textTurn("no tools today"))
	r.withTools().err = errors.New("sandbox down")

	// Act
	_, err := r.do("go")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(r.model.Calls()[0].Tools) != 0 {
		t.Fatal("tools were offered")
	}
}

func TestRunSendsReasoningEffortOnlyToModelsThatAcceptIt(t *testing.T) {
	for name, tc := range map[string]struct {
		model   string
		mapper  func(model, effort string) string
		want    string
		cacheOn bool
	}{
		"reasoning model":    {"o-reason", nil, "high", true},
		"plain model":        {"gpt-plain", nil, "", false},
		"mapped to nearest":  {"o-reason", func(_, e string) string { return "medium" }, "medium", true},
		"model accepts none": {"o-reason", func(_, e string) string { return "" }, "", true},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			r := newRunRig(t, textTurn("ok"))
			h := &r.a.Harness
			h.SupportsReasoning = func(m string) bool { return m == "o-reason" }
			h.ReasoningEffort = tc.mapper
			h.PromptCacheKey = func(m string) bool { return m == "o-reason" }
			run := r.newRun(store.NewRun{Model: tc.model, ReasoningEffort: "high", Message: store.Message{Role: store.RoleUser, Content: "go"}})

			// Act
			if _, err := r.run(run); err != nil {
				t.Fatal(err)
			}

			// Assert
			c := r.model.Calls()[0]
			if c.ReasoningEffort != tc.want {
				t.Errorf("effort %q, want %q", c.ReasoningEffort, tc.want)
			}
			if (c.PromptCacheKey == r.th.ID) != tc.cacheOn {
				t.Errorf("cache key %q", c.PromptCacheKey)
			}
		})
	}
}

func TestRunCountsTheModelsUsageUnderItsOwnName(t *testing.T) {
	// Arrange
	r := newRunRig(t, callTurn("c1", "t", "{}"), llmtest.Turn{Chunks: []*ai.Message{llmtest.WithCachedUsage(&ai.Message{Content: "done"}, 1000, 400, 50)}})
	first := r.model
	_ = first
	r.withTools(tool("t", "r"))

	// Act
	res, err := r.do("go")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	u := res.Usage[0]
	if u.InputTokens != 1000 || u.CachedInputTokens != 400 || u.OutputTokens != 50 || u.TotalTokens != 1050 {
		t.Fatalf("usage %+v", u)
	}
	ev := r.events(agui.Custom + ":" + agui.CustomUsage)
	last := ev[len(ev)-1]["value"].(map[string]any)
	if last["inputTokens"] != 1000 || last["cachedInputTokens"] != 400 || last["contextWindow"] == nil {
		t.Fatalf("usage event %v", last)
	}
}

func TestRunMergesStreamedFragmentsOfParallelCallsByIDWhenAProviderReusesTheIndex(t *testing.T) {
	// Arrange: two calls with distinct ids arrive at the same index.
	r := newRunRig(t,
		llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "ca", "a", `{"x":1}`), llmtest.ToolCall(0, "cb", "b", `{"y":2}`)}},
		textTurn("both done"))
	ta, tb := tool("a", "A"), tool("b", "B")
	r.withTools(ta, tb)

	// Act
	_, err := r.do("go")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ta.calls(), tb.calls()) != `[{"x":1}] [{"y":2}]` {
		t.Fatalf("calls %v %v", ta.calls(), tb.calls())
	}
}
