package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/ai"
	aiagent "github.com/7ar-chat/harness/ai/agent"
	"github.com/7ar-chat/harness/ai/llm"
	"github.com/7ar-chat/harness/core/agui"
)

func TestModelBudgetsFallBackToTheirDefaults(t *testing.T) {
	// Arrange
	zero := &Agent{}
	set := &Agent{ModelTimeout: time.Second, StreamIdleTimeout: 2 * time.Second, CallCeiling: 3 * time.Second}

	// Assert
	if zero.modelTimeout() != 300*time.Second || zero.streamIdle() != 90*time.Second {
		t.Errorf("defaults %v %v", zero.modelTimeout(), zero.streamIdle())
	}
	if set.modelTimeout() != time.Second || set.streamIdle() != 2*time.Second {
		t.Errorf("configured %v %v", set.modelTimeout(), set.streamIdle())
	}
}

func TestStepRetriesDefaultToThreeAndNegativeMeansNever(t *testing.T) {
	for n, want := range map[int]int{0: 3, 1: 1, 7: 7, -1: 0, -5: 0} {
		if got := (&Agent{StepRetries: n}).stepRetries(); got != want {
			t.Errorf("StepRetries %d -> %d, want %d", n, got, want)
		}
	}
}

func TestSleepWaitsOnTheInjectedClockOrTheContext(t *testing.T) {
	// Arrange
	var asked time.Duration
	injected := &Agent{Sleep: func(_ context.Context, d time.Duration) error { asked = d; return errors.New("stop") }}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	// Act
	errInjected := injected.sleep(context.Background(), 5*time.Second)
	errCancelled := (&Agent{}).sleep(cancelled, time.Hour)
	errTimer := (&Agent{}).sleep(context.Background(), time.Millisecond)

	// Assert
	if asked != 5*time.Second || errInjected == nil || errInjected.Error() != "stop" {
		t.Errorf("injected sleep: asked %v err %v", asked, errInjected)
	}
	if !errors.Is(errCancelled, context.Canceled) {
		t.Errorf("cancelled sleep: %v", errCancelled)
	}
	if errTimer != nil {
		t.Errorf("timer sleep: %v", errTimer)
	}
}

func TestReasoningEffortScalesTheBudgetsOnlyForReasoningModels(t *testing.T) {
	for name, tc := range map[string]struct {
		effort    string
		reasoning bool
		want      time.Duration
	}{
		"medium":              {"medium", true, time.Minute},
		"none":                {"", true, time.Minute},
		"high":                {"high", true, 2 * time.Minute},
		"HIGH is case blind":  {"HIGH", true, 2 * time.Minute},
		"xhigh":               {"xhigh", true, 3 * time.Minute},
		"high on plain model": {"high", false, time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			st := newState(5)
			st.a.ModelTimeout, st.a.CallCeiling = time.Minute, time.Minute
			st.a.Harness.SupportsReasoning = func(string) bool { return tc.reasoning }
			st.in.Run.ReasoningEffort = tc.effort

			// Assert
			if got := st.firstChunkBudget(); got != tc.want {
				t.Errorf("first chunk %v, want %v", got, tc.want)
			}
			if got := st.callCeiling(); got != tc.want {
				t.Errorf("ceiling %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCallCeilingDefaultsToTwentyMinutes(t *testing.T) {
	st := newState(5)
	if got := st.callCeiling(); got != 20*time.Minute {
		t.Fatalf("ceiling %v", got)
	}
}

func TestModelFailureClassifiesIntoUserFacingRunErrors(t *testing.T) {
	cause := errors.New("status code: 429 at https://api.internal.example/v1")
	for name, tc := range map[string]struct {
		class llm.ErrorClass
		code  string
		msg   string
	}{
		"timeout":      {llm.ClassTimeout, agui.CodeModelTimeout, MsgModelTimeout},
		"rate limited": {llm.ClassRateLimited, agui.CodeRateLimited, MsgRateLimited},
		"transient":    {llm.ClassTransient, agui.CodeModelError, MsgModelError},
		"fatal":        {llm.ClassFatal, agui.CodeModelError, MsgModelError},
		"overflow":     {llm.ClassOverflow, agui.CodeModelError, MsgModelError},
	} {
		t.Run(name, func(t *testing.T) {
			// Act
			e := modelFailure(&aiagent.CallError{Err: cause, Class: tc.class})

			// Assert
			if e.Code != tc.code || e.Message != tc.msg {
				t.Fatalf("%s: %s", e.Code, e.Message)
			}
			if strings.Contains(e.Message, "internal.example") {
				t.Error("the provider's error reached the client message")
			}
			if !strings.Contains(e.Detail, "internal.example") || !errors.Is(e, cause) {
				t.Errorf("detail %q / cause lost", e.Detail)
			}
		})
	}
}

func TestErrorTextCarriesTheDetailOnlyForLogs(t *testing.T) {
	if got := (&Error{Code: "c", Message: "m"}).Error(); got != "c: m" {
		t.Errorf("%q", got)
	}
	if got := (&Error{Code: "c", Message: "m", Detail: "d"}).Error(); got != "c: m (d)" {
		t.Errorf("%q", got)
	}
}

func TestFinishReasonOfAMissingOutputIsEmpty(t *testing.T) {
	var nilOut *stepOutput
	for _, o := range []*stepOutput{nilOut, {}, {msg: &ai.Message{}}} {
		if got := o.finishReason(); got != "" {
			t.Errorf("finish reason %q", got)
		}
	}
	o := &stepOutput{msg: &ai.Message{ResponseMeta: &ai.ResponseMeta{FinishReason: "length"}}}
	if o.finishReason() != "length" {
		t.Error("finish reason lost")
	}
}

func TestEmptyDetailNamesTheRefusalOrTheFinishReason(t *testing.T) {
	refused := emptyDetail(&stepOutput{refusal: strings.Repeat("no ", 400)})
	plain := emptyDetail(&stepOutput{msg: &ai.Message{ResponseMeta: &ai.ResponseMeta{FinishReason: "stop"}}})
	if !strings.HasPrefix(refused, "refusal: no no") || len(refused) > 600 {
		t.Errorf("refusal detail %q", refused)
	}
	if plain != `empty completion (finish_reason="stop")` {
		t.Errorf("plain detail %q", plain)
	}
}

// transcript drives a translator with chunks and returns the events it wrote.
func newTranslator(w *recordingWriter) (*translator, context.Context) {
	st := newState(5)
	st.w = w
	return &translator{st: st, assistantID: "a1", calls: map[string]*liveCall{}, byIndex: map[int]string{}}, context.Background()
}

func eventTypes(w *recordingWriter) []string {
	var out []string
	for _, e := range w.events {
		s := e.Type()
		if s == agui.ToolCallResult {
			s += ":" + fmt.Sprint(e["toolCallId"])
		}
		out = append(out, s)
	}
	return out
}

func idx(i int) *int { return &i }

func tcChunk(index *int, id, name, args string) *ai.Message {
	return &ai.Message{Role: ai.RoleAssistant, ToolCalls: []ai.ToolCall{{Index: index, ID: id, Function: ai.FunctionCall{Name: name, Arguments: args}}}}
}

func TestTranslatorEndsTheTextBeforeTheFirstToolCallStarts(t *testing.T) {
	// Arrange
	w := &recordingWriter{}
	tr, ctx := newTranslator(w)

	// Act
	for _, c := range []*ai.Message{{Content: "Let me "}, {Content: "look."}, tcChunk(idx(0), "c1", "read_file", `{"path":`), tcChunk(idx(0), "", "", `"/a"}`)} {
		if err := tr.Chunk(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := tr.Settle(ctx, false); err != nil {
		t.Fatal(err)
	}

	// Assert
	want := "TEXT_MESSAGE_START TEXT_MESSAGE_CONTENT TEXT_MESSAGE_CONTENT TEXT_MESSAGE_END TOOL_CALL_START TOOL_CALL_ARGS TOOL_CALL_ARGS TOOL_CALL_END"
	if got := strings.Join(eventTypes(w), " "); got != want {
		t.Fatalf("events:\n got %s\nwant %s", got, want)
	}
	if len(tr.merged) != 1 || tr.merged[0].ID != "c1" || tr.merged[0].Function.Arguments != `{"path":"/a"}` {
		t.Fatalf("merged %+v", tr.merged)
	}
}

func TestTranslatorEndsEachToolCallBeforeTheNextStarts(t *testing.T) {
	// Arrange
	w := &recordingWriter{}
	tr, ctx := newTranslator(w)

	// Act: parallel calls whose argument fragments interleave by index.
	for _, c := range []*ai.Message{
		tcChunk(idx(0), "c1", "read_file", `{"a":`), tcChunk(idx(1), "c2", "list_files", `{"b":`), tcChunk(idx(0), "", "", `1}`), tcChunk(idx(1), "", "", `2}`),
	} {
		_ = tr.Chunk(ctx, c)
	}
	_ = tr.Settle(ctx, false)

	// Assert
	var started, ended []string
	open := ""
	for _, e := range w.events {
		id, _ := e["toolCallId"].(string)
		switch e.Type() {
		case agui.ToolCallStart:
			if open != "" {
				t.Fatalf("%s started while %s is open", id, open)
			}
			open = id
			started = append(started, id)
		case agui.ToolCallEnd:
			ended = append(ended, id)
			open = ""
		}
	}
	if fmt.Sprint(started) != "[c1 c2]" || fmt.Sprint(ended) != "[c1 c2]" {
		t.Fatalf("started %v ended %v", started, ended)
	}
	if len(tr.merged) != 2 || tr.merged[0].Function.Arguments != `{"a":1}` || tr.merged[1].Function.Arguments != `{"b":2}` {
		t.Fatalf("merged %+v", tr.merged)
	}
}

func TestTranslatorKeepsTwoCallsApartWhenAProviderReusesAnIndex(t *testing.T) {
	// Arrange
	w := &recordingWriter{}
	tr, ctx := newTranslator(w)

	// Act
	_ = tr.Chunk(ctx, tcChunk(idx(0), "c1", "read_file", `{"a":1}`))
	_ = tr.Chunk(ctx, tcChunk(idx(0), "c2", "list_files", `{"b":2}`))
	_ = tr.Settle(ctx, false)

	// Assert
	if len(tr.merged) != 2 || tr.merged[0].ID != "c1" || tr.merged[1].ID != "c2" || tr.merged[1].Function.Arguments != `{"b":2}` {
		t.Fatalf("merged %+v", tr.merged)
	}
}

func TestTranslatorMergesIndexlessFragmentsOfOneCall(t *testing.T) {
	// Arrange: no Index, no ID on the continuation: the first fragment of a chunk continues the previous call.
	w := &recordingWriter{}
	tr, ctx := newTranslator(w)

	// Act
	_ = tr.Chunk(ctx, tcChunk(nil, "c1", "read_file", `{"pa`))
	_ = tr.Chunk(ctx, tcChunk(nil, "", "", `th":"/a"}`))
	_ = tr.Settle(ctx, false)

	// Assert
	if len(tr.merged) != 1 || tr.merged[0].Function.Arguments != `{"path":"/a"}` {
		t.Fatalf("merged %+v", tr.merged)
	}
}

func TestTranslatorGivesACallWithoutAnIDOneAndStartsItAtTheEnd(t *testing.T) {
	// Arrange
	w := &recordingWriter{}
	tr, ctx := newTranslator(w)

	// Act: the provider never sent an id.
	_ = tr.Chunk(ctx, tcChunk(idx(0), "", "read_file", `{}`))
	_ = tr.Settle(ctx, false)

	// Assert
	if len(tr.merged) != 1 || !strings.HasPrefix(tr.merged[0].ID, "call_") || tr.merged[0].Type != "function" {
		t.Fatalf("merged %+v", tr.merged)
	}
	if got := strings.Join(eventTypes(w), " "); got != "TOOL_CALL_START TOOL_CALL_ARGS TOOL_CALL_END" {
		t.Fatalf("events %s", got)
	}
	if ids := tr.announced(); len(ids) != 1 || ids[0] != tr.merged[0].ID {
		t.Fatalf("announced %v", ids)
	}
}

func TestTranslatorClosesReasoningBeforeTextAndKeepsItsText(t *testing.T) {
	// Arrange
	w := &recordingWriter{}
	tr, ctx := newTranslator(w)

	// Act
	_ = tr.Chunk(ctx, &ai.Message{ReasoningContent: "think "})
	_ = tr.Chunk(ctx, &ai.Message{ReasoningContent: "more"})
	_ = tr.Chunk(ctx, &ai.Message{Content: "Answer"})
	_ = tr.Settle(ctx, false)

	// Assert
	want := "REASONING_START REASONING_MESSAGE_START REASONING_MESSAGE_CONTENT REASONING_MESSAGE_CONTENT REASONING_MESSAGE_END REASONING_END TEXT_MESSAGE_START TEXT_MESSAGE_CONTENT TEXT_MESSAGE_END"
	if got := strings.Join(eventTypes(w), " "); got != want {
		t.Fatalf("events:\n got %s\nwant %s", got, want)
	}
	if tr.reasoning.String() != "think more" || tr.reasoningID == "" {
		t.Fatalf("reasoning %q id %q", tr.reasoning.String(), tr.reasoningID)
	}
}

func TestTranslatorDropsAnUnannouncedCallOfABrokenStream(t *testing.T) {
	// Arrange: a cancelled stream stopped after a call's id but before its name.
	w := &recordingWriter{}
	tr, ctx := newTranslator(w)
	_ = tr.Chunk(ctx, tcChunk(idx(0), "c1", "read_file", `{}`))
	_ = tr.Chunk(ctx, tcChunk(idx(1), "c2", "", `{"x"`))

	// Act
	_ = tr.Settle(ctx, true)

	// Assert
	if tr.merged != nil {
		t.Fatalf("a broken stream returned calls: %+v", tr.merged)
	}
	if got := strings.Join(eventTypes(w), " "); got != "TOOL_CALL_START TOOL_CALL_ARGS TOOL_CALL_END" {
		t.Fatalf("events %s", got)
	}
}

func TestTranslatorAbortGivesEveryAnnouncedCallAnErrorResult(t *testing.T) {
	for name, tc := range map[string]struct {
		retried bool
		content string
		retry   bool
	}{
		"retried": {true, StreamRetriedResult, true},
		"failed":  {false, StreamFailedResult, false},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			w := &recordingWriter{}
			tr, ctx := newTranslator(w)
			_ = tr.Chunk(ctx, &ai.Message{Content: "partial"})
			_ = tr.Chunk(ctx, tcChunk(idx(0), "c1", "read_file", `{`))
			_ = tr.Chunk(ctx, tcChunk(idx(1), "c2", "list_files", `{`))
			w.events = nil

			// Act
			if err := tr.Abort(ctx, tc.retried); err != nil {
				t.Fatal(err)
			}

			// Assert
			var results []agui.Event
			for _, e := range w.events {
				if e.Type() == agui.ToolCallResult {
					results = append(results, e)
				}
			}
			if len(results) != 2 || results[0]["toolCallId"] != "c1" || results[1]["toolCallId"] != "c2" {
				t.Fatalf("results %+v", results)
			}
			for _, r := range results {
				md := r["metadata"].(map[string]any)
				if r["content"] != tc.content || md["isError"] != true || (md["retried"] == true) != tc.retry {
					t.Errorf("result %+v", r)
				}
			}
			if types := eventTypes(w); types[0] != "TOOL_CALL_END" {
				t.Errorf("the open call was not ended first: %v", types)
			}
		})
	}
}

func TestTranslatorStopsAtTheFirstEventLogFailure(t *testing.T) {
	// Arrange
	w := &recordingWriter{failOn: agui.TextMessageContent}
	tr, ctx := newTranslator(w)

	// Act
	err := tr.Chunk(ctx, &ai.Message{Content: "hello"})

	// Assert
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != agui.CodeInternal || ae.Message != MsgInternal || !strings.Contains(ae.Detail, "event log") {
		t.Fatalf("err %v", err)
	}
}

func TestTranslatorIgnoresANilChunk(t *testing.T) {
	w := &recordingWriter{}
	tr, ctx := newTranslator(w)
	if err := tr.Chunk(ctx, nil); err != nil || len(w.events) != 0 {
		t.Fatalf("err %v events %v", err, w.events)
	}
}
