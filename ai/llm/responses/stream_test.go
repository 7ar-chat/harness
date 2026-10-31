package responses_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/responses"
)

// finalOf returns the last chunk of a stream: the one that carries the finish reason.
func finalOf(t *testing.T, chunks []*ai.Message) *ai.Message {
	t.Helper()
	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
	return chunks[len(chunks)-1]
}

func textOf(chunks []*ai.Message) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString(c.Content)
	}
	return b.String()
}

func TestReasoningSummaryPartsAreSeparatedByABlankLine(t *testing.T) {
	// A new summary part of the same reasoning item starts a new paragraph; the first part and a repeated index do not.
	// Arrange
	body := event("response.reasoning_summary_text.delta", map[string]any{"item_id": "rs", "summary_index": 0, "delta": "one "}) +
		event("response.reasoning_summary_text.delta", map[string]any{"item_id": "rs", "summary_index": 0, "delta": "more"}) +
		event("response.reasoning_summary_text.delta", map[string]any{"item_id": "rs", "summary_index": 1, "delta": "two"}) +
		event("response.reasoning_summary_text.delta", map[string]any{"item_id": "rs", "summary_index": 1, "delta": ""}) +
		event("response.reasoning_summary_text.delta", map[string]any{"item_id": "other", "summary_index": 1, "delta": "x"}) +
		completed(nil)
	url, _ := serve(t, stream(body))

	// Act
	out, err := newModel(t, url).Generate(context.Background(), hi())

	// Assert
	if err != nil || out.ReasoningContent != "one more\n\ntwox" {
		t.Fatalf("reasoning %q err %v", out.ReasoningContent, err)
	}
	if out.Content != "" {
		t.Fatalf("reasoning leaked into the answer: %q", out.Content)
	}
}

func TestRefusalTextGoesToTheCallbackNotTheAnswer(t *testing.T) {
	// Refusal deltas are handed to OnRefusal with the call context and never appear in the message; without a callback they are dropped.
	// Arrange
	body := event("response.refusal.delta", map[string]any{"delta": "I can't "}) +
		event("response.refusal.delta", map[string]any{"delta": ""}) +
		event("response.refusal.delta", map[string]any{"delta": "do that."}) +
		completed(nil)
	url, _ := serve(t, stream(body))
	type ctxKey struct{}
	var gotText strings.Builder
	var gotCtx any
	m := newModel(t, url, func(c *responses.Config) {
		c.OnRefusal = func(ctx context.Context, text string) {
			gotText.WriteString(text)
			gotCtx = ctx.Value(ctxKey{})
		}
	})
	plain := newModel(t, url)

	// Act
	out, err := m.Generate(context.WithValue(context.Background(), ctxKey{}, "call-1"), hi())
	outPlain, errPlain := plain.Generate(context.Background(), hi())

	// Assert
	if err != nil || errPlain != nil {
		t.Fatal(err, errPlain)
	}
	if gotText.String() != "I can't do that." || gotCtx != "call-1" {
		t.Fatalf("refusal %q ctx %v", gotText.String(), gotCtx)
	}
	if out.Content != "" || outPlain.Content != "" || out.ResponseMeta.FinishReason != "stop" {
		t.Fatalf("out %+v plain %+v", out, outPlain)
	}
}

func TestFunctionCallsKeepTheirRankAndIdentity(t *testing.T) {
	// Parallel calls get a stable index in announcement order; the call id and name ride on the first fragment only, and argument deltas find their call by item id or, failing that, by output index.
	// Arrange
	add := func(idx int, id, call, name string) string {
		return event("response.output_item.added", map[string]any{"output_index": idx, "item": map[string]any{"type": "function_call", "id": id, "call_id": call, "name": name}})
	}
	body := add(3, "fc_a", "call_a", "alpha") +
		add(5, "fc_b", "call_b", "beta") +
		event("response.function_call_arguments.delta", map[string]any{"item_id": "fc_b", "output_index": 5, "delta": `{"b":`}) +
		event("response.function_call_arguments.delta", map[string]any{"item_id": "unknown-id", "output_index": 3, "delta": `{"a":1}`}) +
		event("response.function_call_arguments.delta", map[string]any{"item_id": "nobody", "output_index": 9, "delta": `lost`}) +
		event("response.function_call_arguments.delta", map[string]any{"item_id": "fc_b", "delta": ""}) +
		event("response.function_call_arguments.delta", map[string]any{"item_id": "fc_b", "delta": `2}`}) +
		completed(nil)
	url, _ := serve(t, stream(body))

	// Act
	chunks, err := streamAll(t, newModel(t, url), hi())
	out, cerr := ai.ConcatMessages(chunks)

	// Assert
	if err != nil || cerr != nil {
		t.Fatal(err, cerr)
	}
	if len(out.ToolCalls) != 2 {
		t.Fatalf("calls %+v", out.ToolCalls)
	}
	a, b := out.ToolCalls[0], out.ToolCalls[1]
	if *a.Index != 0 || a.ID != "call_a" || a.Function.Name != "alpha" || a.Function.Arguments != `{"a":1}` ||
		*b.Index != 1 || b.ID != "call_b" || b.Function.Name != "beta" || b.Function.Arguments != `{"b":2}` {
		t.Fatalf("calls %+v %+v", a, b)
	}
	// Only the first fragment of a call announces id and name.
	for _, c := range chunks {
		for _, tc := range c.ToolCalls {
			if tc.Function.Arguments != "" && (tc.ID != "" || tc.Function.Name != "") {
				t.Fatalf("argument fragment repeats identity: %+v", tc)
			}
		}
	}
	if finalOf(t, chunks).ResponseMeta.FinishReason != "tool_calls" {
		t.Fatalf("meta %+v", finalOf(t, chunks).ResponseMeta)
	}
}

func TestArgumentsAnnouncedWithTheCallAreNotDuplicatedByLaterEvents(t *testing.T) {
	// When the arguments arrive whole on output_item.added, the .done events that repeat them must not append a second copy.
	// Arrange
	item := map[string]any{"type": "function_call", "id": "fc", "call_id": "c", "name": "f", "arguments": `{"x":1}`}
	body := event("response.output_item.added", map[string]any{"output_index": 0, "item": item}) +
		event("response.function_call_arguments.done", map[string]any{"item_id": "fc", "arguments": `{"x":1}`}) +
		event("response.output_item.done", map[string]any{"output_index": 0, "item": item}) +
		completed(nil)
	url, _ := serve(t, stream(body))

	// Act
	out, err := newModel(t, url).Generate(context.Background(), hi())

	// Assert
	if err != nil || len(out.ToolCalls) != 1 || out.ToolCalls[0].Function.Arguments != `{"x":1}` {
		t.Fatalf("err %v calls %+v", err, out.ToolCalls)
	}
}

func TestArgumentsFromDoneEventsFillACallThatStreamedNone(t *testing.T) {
	// A call whose arguments never streamed gets them from arguments.done (matched by item id or output index), once; arguments.done without a known call is ignored.
	// Arrange
	add := func(idx int, id, call string) string {
		return event("response.output_item.added", map[string]any{"output_index": idx, "item": map[string]any{"type": "function_call", "id": id, "call_id": call, "name": "f"}})
	}
	body := add(0, "fc_a", "call_a") + add(1, "fc_b", "call_b") +
		event("response.function_call_arguments.done", map[string]any{"item_id": "fc_a", "arguments": `{"a":1}`}) +
		event("response.function_call_arguments.done", map[string]any{"item_id": "fc_a", "arguments": `{"a":"again"}`}) +
		event("response.function_call_arguments.done", map[string]any{"output_index": 1, "arguments": `{"b":2}`}) +
		event("response.function_call_arguments.done", map[string]any{"item_id": "ghost", "output_index": 7, "arguments": `{"g":0}`}) +
		completed(nil)
	url, _ := serve(t, stream(body))

	// Act
	out, err := newModel(t, url).Generate(context.Background(), hi())

	// Assert
	if err != nil || len(out.ToolCalls) != 2 ||
		out.ToolCalls[0].Function.Arguments != `{"a":1}` || out.ToolCalls[1].Function.Arguments != `{"b":2}` {
		t.Fatalf("err %v calls %+v", err, out.ToolCalls)
	}
}

func TestItemDoneRepairsWhatTheDeltasMissed(t *testing.T) {
	// output_item.done completes a call whose arguments never streamed, announces a call never announced, and delivers message text that was never streamed; text that did stream is not repeated.
	// Arrange
	body := event("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc_a", "call_id": "call_a", "name": "alpha"}}) +
		event("response.output_item.done", map[string]any{"output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc_a", "call_id": "call_a", "name": "alpha", "arguments": `{"a":1}`}}) +
		event("response.output_item.done", map[string]any{"output_index": 1, "item": map[string]any{"type": "function_call", "id": "fc_b", "call_id": "call_b", "name": "beta", "arguments": `{"b":2}`}}) +
		event("response.output_item.done", map[string]any{"output_index": 2, "item": map[string]any{"type": "message", "id": "msg_1", "content": []any{
			map[string]any{"type": "output_text", "text": "late "},
			map[string]any{"type": "refusal", "refusal": "no"},
			map[string]any{"type": "output_text", "text": "text"},
			map[string]any{"type": "output_text", "text": ""}}}}) +
		event("response.output_text.delta", map[string]any{"item_id": "msg_2", "delta": "streamed"}) +
		event("response.output_item.done", map[string]any{"output_index": 3, "item": map[string]any{"type": "message", "id": "msg_2", "content": []any{
			map[string]any{"type": "output_text", "text": "streamed"}}}}) +
		event("response.output_item.done", map[string]any{"output_index": 4}) +
		event("response.output_item.done", map[string]any{"output_index": 5, "item": map[string]any{"type": "web_search_call", "id": "ws"}}) +
		completed(nil)
	url, _ := serve(t, stream(body))

	// Act
	out, err := newModel(t, url).Generate(context.Background(), hi())

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if out.Content != "late textstreamed" {
		t.Fatalf("content %q", out.Content)
	}
	if len(out.ToolCalls) != 2 || out.ToolCalls[0].ID != "call_a" || out.ToolCalls[0].Function.Arguments != `{"a":1}` ||
		out.ToolCalls[1].ID != "call_b" || *out.ToolCalls[1].Index != 1 || out.ToolCalls[1].Function.Arguments != `{"b":2}` {
		t.Fatalf("calls %+v", out.ToolCalls)
	}
}

func TestReasoningItemsComeFromTheStreamOrTheFinalResponseOnce(t *testing.T) {
	// A reasoning item seen on output_item.done is kept once even when the final response lists it again; items without an id or encrypted content are discarded; when no item streamed, those of the final response are used.
	// Arrange
	rs := func(id, enc string) map[string]any {
		return map[string]any{"type": "reasoning", "id": id, "encrypted_content": enc, "summary": []any{map[string]any{"type": "summary_text", "text": "s-" + id}}}
	}
	streamed := event("response.output_item.done", map[string]any{"item": rs("rs_1", "e1")}) +
		event("response.output_item.done", map[string]any{"item": rs("rs_1", "e1-dup")}) +
		event("response.output_item.done", map[string]any{"item": rs("", "e")}) +
		event("response.output_item.done", map[string]any{"item": rs("rs_empty", "")}) +
		completed(map[string]any{"output": []any{rs("rs_2", "ignored: streamed items win")}})
	onlyFinal := completed(map[string]any{"output": []any{rs("rs_3", "e3"), map[string]any{"type": "message", "id": "m"}, rs("rs_4", "e4")}})
	url, _ := serve(t, stream(streamed), stream(onlyFinal))
	m := newModel(t, url)

	// Act
	first, err1 := m.Generate(context.Background(), hi())
	second, err2 := m.Generate(context.Background(), hi())

	// Assert
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if got := responses.ItemsOf(first); len(got) != 1 || got[0].ID != "rs_1" || got[0].EncryptedContent != "e1" ||
		got[0].Model != "gpt-x" || len(got[0].Summary) != 1 || got[0].Summary[0].Text != "s-rs_1" {
		t.Fatalf("streamed items %+v", got)
	}
	if got := responses.ItemsOf(second); len(got) != 2 || got[0].ID != "rs_3" || got[1].ID != "rs_4" {
		t.Fatalf("final-response items %+v", got)
	}
}

func TestUsageFiguresAreMappedIncludingCacheAndReasoning(t *testing.T) {
	// The terminal event's usage is reported with cached/written prompt tokens and reasoning tokens; a response without usage reports none.
	// Arrange
	withUsage := completed(map[string]any{"usage": map[string]any{
		"input_tokens": 100, "input_tokens_details": map[string]any{"cached_tokens": 60, "cache_write_tokens": 7},
		"output_tokens": 40, "output_tokens_details": map[string]any{"reasoning_tokens": 25}, "total_tokens": 140}})
	url, _ := serve(t, stream(withUsage), stream(completed(nil)))
	m := newModel(t, url)

	// Act
	a, errA := m.Generate(context.Background(), hi())
	b, errB := m.Generate(context.Background(), hi())

	// Assert
	if errA != nil || errB != nil {
		t.Fatal(errA, errB)
	}
	u := a.ResponseMeta.Usage
	if u == nil || u.PromptTokens != 100 || u.PromptTokenDetails.CachedTokens != 60 || u.PromptTokenDetails.CacheWriteTokens != 7 ||
		u.CompletionTokens != 40 || u.CompletionTokenDetails.ReasoningTokens != 25 || u.TotalTokens != 140 {
		t.Fatalf("usage %+v", u)
	}
	if b.ResponseMeta == nil || b.ResponseMeta.Usage != nil || b.ResponseMeta.FinishReason != "stop" {
		t.Fatalf("meta %+v, want stop without usage", b.ResponseMeta)
	}
}

func TestIncompleteResponsesMapToLengthOrContentFilter(t *testing.T) {
	// response.incomplete is a truncation (length) unless the provider says it was filtered; the text produced so far is kept either way.
	// Arrange
	incomplete := func(details map[string]any) string {
		r := map[string]any{"status": "incomplete", "usage": map[string]any{"output_tokens": 9}}
		if details != nil {
			r["incomplete_details"] = details
		}
		return event("response.output_text.delta", map[string]any{"item_id": "m", "delta": "cut o"}) +
			event("response.incomplete", map[string]any{"response": r})
	}
	cases := []struct {
		name string
		body string
		want string
	}{
		{"max tokens", incomplete(map[string]any{"reason": "max_output_tokens"}), "length"},
		{"no details", incomplete(nil), "length"},
		{"content filter", incomplete(map[string]any{"reason": "Content_Filter"}), "content_filter"},
		{"no response object", event("response.incomplete", nil), "length"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, _ := serve(t, stream(tc.body))

			// Act
			out, err := newModel(t, url).Generate(context.Background(), hi())

			// Assert
			if err != nil || out.ResponseMeta.FinishReason != tc.want {
				t.Fatalf("err %v meta %+v, want %s", err, out.ResponseMeta, tc.want)
			}
		})
	}
}

func TestIncompleteAfterToolCallsStillReportsLength(t *testing.T) {
	// A truncated response must not look like a finished tool-calling step, even though calls were announced.
	// Arrange
	body := event("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc", "call_id": "c", "name": "f"}}) +
		event("response.incomplete", map[string]any{"response": map[string]any{"incomplete_details": map[string]any{"reason": "max_output_tokens"}}})
	url, _ := serve(t, stream(body))

	// Act
	out, err := newModel(t, url).Generate(context.Background(), hi())

	// Assert
	if err != nil || out.ResponseMeta.FinishReason != "length" {
		t.Fatalf("err %v meta %+v", err, out.ResponseMeta)
	}
}

func TestFailedResponseIsAnErrorUnlessTheContentWasFiltered(t *testing.T) {
	// response.failed fails the call with the provider's code (which decides its retry class); a content-filter failure ends the answer normally with finish_reason content_filter and its usage.
	// Arrange
	failed := func(resp any) string { return event("response.failed", map[string]any{"response": resp}) }
	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
		wantFilter bool
	}{
		{"rate limit", failed(map[string]any{"error": map[string]any{"code": "rate_limit_exceeded", "message": "slow"}}), 429, "rate_limit_exceeded", false},
		{"server error", failed(map[string]any{"error": map[string]any{"code": "server_error", "message": "oops"}}), 500, "server_error", false},
		{"numeric code", failed(map[string]any{"error": map[string]any{"code": 503, "type": "unavailable", "message": "later"}}), 500, "503", false},
		{"null code", failed(map[string]any{"error": map[string]any{"code": nil, "type": "invalid_request_error", "message": "bad"}}), 400, "", false},
		{"unknown code", failed(map[string]any{"error": map[string]any{"code": "mystery", "message": "?"}}), 0, "mystery", false},
		{"no error object", failed(map[string]any{"status": "failed"}), 500, "server_error", false},
		{"no response", event("response.failed", nil), 500, "server_error", false},
		{"filtered", failed(map[string]any{"error": map[string]any{"code": "content_filter", "message": "blocked"}, "usage": map[string]any{"input_tokens": 4, "total_tokens": 4}}), 0, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, _ := serve(t, stream(event("response.output_text.delta", map[string]any{"item_id": "m", "delta": "x"})+tc.body))

			// Act
			chunks, err := streamAll(t, newModel(t, url), hi())

			// Assert
			if tc.wantFilter {
				last := finalOf(t, chunks)
				if err != nil || last.ResponseMeta.FinishReason != "content_filter" || last.ResponseMeta.Usage.PromptTokens != 4 {
					t.Fatalf("err %v last %+v", err, last.ResponseMeta)
				}
				return
			}
			var apiErr *responses.APIError
			if !errors.As(err, &apiErr) || apiErr.Status != tc.wantStatus || apiErr.Code != tc.wantCode {
				t.Fatalf("err %v, want status %d code %q", err, tc.wantStatus, tc.wantCode)
			}
		})
	}
}

func TestErrorEventsCarryTheirCodeTopLevelOrNested(t *testing.T) {
	// An `error` event ends the stream with its code, read from the event itself or from a nested error object, which wins.
	// Arrange
	cases := []struct {
		name       string
		fields     map[string]any
		wantStatus int
		wantCode   string
		wantMsg    string
	}{
		{"top level", map[string]any{"code": "context_length_exceeded", "message": "too long"}, 400, "context_length_exceeded", "too long"},
		{"nested", map[string]any{"error": map[string]any{"type": "too_many_requests", "code": "x", "message": "inner"}, "code": "server_error", "message": "outer"}, 429, "x", "inner"},
		{"empty", map[string]any{}, 0, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, _ := serve(t, stream(event("error", tc.fields)))

			// Act
			_, err := streamAll(t, newModel(t, url), hi())

			// Assert
			var apiErr *responses.APIError
			if !errors.As(err, &apiErr) || apiErr.Status != tc.wantStatus || apiErr.Code != tc.wantCode || apiErr.Message != tc.wantMsg {
				t.Fatalf("err %#v, want %d %q %q", err, tc.wantStatus, tc.wantCode, tc.wantMsg)
			}
		})
	}
}

func TestStreamEndingBeforeTheTerminalEventIsATransientTruncation(t *testing.T) {
	// A stream that stops (cleanly) without response.completed reports ErrStreamEnded, which is an unexpected EOF so it is retried, and the text before the cut was still delivered.
	// Arrange
	url, _ := serve(t, stream(event("response.output_text.delta", map[string]any{"item_id": "m", "delta": "par"})))

	// Act
	chunks, err := streamAll(t, newModel(t, url), hi())

	// Assert
	if !errors.Is(err, responses.ErrStreamEnded) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err %v", err)
	}
	if textOf(chunks) != "par" {
		t.Fatalf("text before the cut %q", textOf(chunks))
	}
}

func TestEventWithoutTrailingBlankLineIsStillHandled(t *testing.T) {
	// A server that closes right after the last data line (no blank line) still gets its terminal event honoured.
	// Arrange
	url, _ := serve(t, stream("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"total_tokens\":3}}}"))

	// Act
	out, err := newModel(t, url).Generate(context.Background(), hi())

	// Assert
	if err != nil || out.ResponseMeta.Usage == nil || out.ResponseMeta.Usage.TotalTokens != 3 {
		t.Fatalf("err %v out %+v", err, out)
	}
}

func TestSSEFramingQuirksAreTolerated(t *testing.T) {
	// CRLF lines, comments, event: lines, data without a space, [DONE], garbage JSON and unknown event types are skipped; a payload split over several data: lines is joined with newlines into one JSON value.
	// Arrange
	body := ": keep-alive\r\n\r\n" +
		"event: response.created\r\ndata:{\"type\":\"response.created\"}\r\n\r\n" +
		"data: not json\n\n" +
		"data: [DONE]\n\n" +
		"data: {\"type\":\"response.some_new_event\",\"x\":1}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\ndata: \"item_id\":\"m\",\"delta\":\"he\"}\n\n" +
		"data:{\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"delta\":\"llo\"}\r\n\r\n" +
		"retry: 100\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{}}\n\n"
	url, _ := serve(t, stream(body))

	// Act
	out, err := newModel(t, url).Generate(context.Background(), hi())

	// Assert
	if err != nil || out.Content != "hello" || out.ResponseMeta.FinishReason != "stop" {
		t.Fatalf("err %v out %+v", err, out)
	}
}

func TestNothingAfterTheTerminalEventIsRead(t *testing.T) {
	// The first terminal event ends the answer: later events (a stray error, more text) do not change it.
	// Arrange
	url, _ := serve(t, stream(completed(nil)+
		event("response.output_text.delta", map[string]any{"item_id": "m", "delta": "late"})+
		event("error", map[string]any{"code": "server_error"})))

	// Act
	out, err := newModel(t, url).Generate(context.Background(), hi())

	// Assert
	if err != nil || out.Content != "" || out.ResponseMeta.FinishReason != "stop" {
		t.Fatalf("err %v out %+v", err, out)
	}
}

func TestAbortedConnectionSurfacesAsAReadError(t *testing.T) {
	// A connection torn down mid-stream (not a clean EOF) fails the stream with a transport error after the chunks that arrived.
	// Arrange
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", "100000")
		_, _ = w.Write([]byte(event("response.output_text.delta", map[string]any{"item_id": "m", "delta": "par"})))
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(ts.Close)

	// Act
	chunks, err := streamAll(t, newModel(t, ts.URL+"/v1/responses"), hi())

	// Assert
	if err == nil || errors.Is(err, responses.ErrStreamEnded) || textOf(chunks) != "par" {
		t.Fatalf("err %v text %q, want the transport error after \"par\"", err, textOf(chunks))
	}
}
