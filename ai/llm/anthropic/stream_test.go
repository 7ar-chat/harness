package anthropic_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm"
	"github.com/7ar-chat/harness/ai/llm/anthropic"
)

// events renders SSE data lines the way the API frames them.
func events(data ...string) string {
	var b strings.Builder
	for _, d := range data {
		b.WriteString("event: x\ndata: " + d + "\n\n")
	}
	return b.String()
}

func serve(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }
}

func recvAll(t *testing.T, sr *ai.StreamReader[*ai.Message]) ([]*ai.Message, error) {
	t.Helper()
	defer sr.Close()
	var out []*ai.Message
	for {
		c, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, c)
	}
}

func TestStreamMapsStopReasonsToFinishReasons(t *testing.T) {
	for stop, want := range map[string]string{
		"end_turn":                      "stop",
		"stop_sequence":                 "stop",
		"":                              "stop",
		"tool_use":                      "tool_calls",
		"max_tokens":                    "length",
		"model_context_window_exceeded": "length",
		"refusal":                       "content_filter",
	} {
		t.Run(stop, func(t *testing.T) {
			// Arrange
			delta := `{"type":"message_delta","delta":{"stop_reason":"` + stop + `"},"usage":{"output_tokens":5}}`
			m := newModel(t, serve(events(delta, `{"type":"message_stop"}`)), anthropic.Config{})

			// Act
			msg, err := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")})

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			if got := msg.ResponseMeta.FinishReason; got != want {
				t.Fatalf("stop_reason %q: finish reason %q, want %q", stop, got, want)
			}
		})
	}
}

func TestStreamKeepsThinkingApartFromTheAnswer(t *testing.T) {
	// Arrange
	m := newModel(t, serve(events(
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hm, "}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"yes."}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Answer"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":""}}`,
		`{"type":"message_stop"}`,
	)), anthropic.Config{})

	// Act
	msg, err := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if msg.ReasoningContent != "hm, yes." || msg.Content != "Answer" {
		t.Fatalf("reasoning %q content %q", msg.ReasoningContent, msg.Content)
	}
}

func TestStreamNumbersToolCallsInOrderWhateverTheBlockIndexes(t *testing.T) {
	// Arrange: text at block 0, tool calls at blocks 2 and 5, their arguments interleaved.
	m := newModel(t, serve(events(
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"t_a","name":"alpha"}}`,
		`{"type":"content_block_start","index":5,"content_block":{"type":"tool_use","id":"t_b","name":"beta"}}`,
		`{"type":"content_block_delta","index":5,"delta":{"type":"input_json_delta","partial_json":"{\"b\":"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
		`{"type":"content_block_delta","index":5,"delta":{"type":"input_json_delta","partial_json":"2}"}}`,
		`{"type":"content_block_delta","index":9,"delta":{"type":"input_json_delta","partial_json":"{\"stray\":true}"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":""}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`{"type":"message_stop"}`,
	)), anthropic.Config{})

	// Act
	msg, err := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.ToolCalls) != 2 {
		t.Fatalf("tool calls %+v", msg.ToolCalls)
	}
	a, b := msg.ToolCalls[0], msg.ToolCalls[1]
	if a.ID != "t_a" || a.Function.Name != "alpha" || a.Function.Arguments != `{"a":1}` {
		t.Errorf("first call %+v", a)
	}
	if b.ID != "t_b" || b.Function.Name != "beta" || b.Function.Arguments != `{"b":2}` {
		t.Errorf("second call %+v", b)
	}
}

func TestStreamReportsThePromptAsTheSumOfFreshAndCachedTokens(t *testing.T) {
	// Arrange: the final message_delta repeats the input counts (as some proxies do) with different cache numbers.
	m := newModel(t, serve(events(
		`{"type":"message_start","message":{"usage":{"input_tokens":5,"cache_read_input_tokens":1,"cache_creation_input_tokens":2,"output_tokens":1}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":20,"cache_read_input_tokens":100,"cache_creation_input_tokens":10,"output_tokens":7}}`,
		`{"type":"message_stop"}`,
	)), anthropic.Config{})

	// Act
	msg, err := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	u := msg.ResponseMeta.Usage
	if u.PromptTokens != 130 || u.CompletionTokens != 7 || u.TotalTokens != 137 ||
		u.PromptTokenDetails.CachedTokens != 100 || u.PromptTokenDetails.CacheWriteTokens != 10 {
		t.Fatalf("usage %+v", u)
	}
}

func TestStreamKeepsStartUsageWhenTheClosingDeltaOnlyCountsOutput(t *testing.T) {
	// Arrange
	m := newModel(t, serve(events(
		`{"type":"message_start","message":{"usage":{"input_tokens":5,"cache_read_input_tokens":50,"cache_creation_input_tokens":0}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`,
		`{"type":"message_stop"}`,
	)), anthropic.Config{})

	// Act
	msg, err := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if u := msg.ResponseMeta.Usage; u.PromptTokens != 55 || u.PromptTokenDetails.CachedTokens != 50 || u.CompletionTokens != 9 {
		t.Fatalf("usage %+v", u)
	}
}

func TestStreamSkipsNoiseAndMalformedEvents(t *testing.T) {
	// Arrange
	body := ": keep-alive comment\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"data: {not json\n\n" +
		"data:{\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" + // no space after the colon
		"data: {\"type\":\"content_block_delta\",\"index\":0}\n\n" + // no delta
		"data: {\"type\":\"content_block_start\",\"index\":0}\n\n" + // no block
		"data: {\"type\":\"message_start\"}\n\n" + // no message
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"future_delta\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	m := newModel(t, serve(body), anthropic.Config{})

	// Act
	msg, err := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "ok" {
		t.Fatalf("content %q", msg.Content)
	}
}

func TestStreamCutOffBeforeMessageStopIsATransientError(t *testing.T) {
	// Arrange
	m := newModel(t, serve(events(
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half an ans"}}`,
	)), anthropic.Config{})

	// Act
	sr, err := m.Stream(context.Background(), []*ai.Message{ai.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := recvAll(t, sr)

	// Assert
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err %v, want unexpected EOF", err)
	}
	if llm.Classify(err) != llm.ClassTransient {
		t.Errorf("a truncated stream classified %v, want transient", llm.Classify(err))
	}
	if len(chunks) != 1 || chunks[0].Content != "half an ans" {
		t.Errorf("chunks before the cut %+v", chunks)
	}
}

func TestStreamDeliversChunksBeforeAnInStreamError(t *testing.T) {
	for _, tc := range []struct {
		name, errEvent string
		status         int
		class          llm.ErrorClass
	}{
		{"overloaded", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, 529, llm.ClassTransient},
		{"rate limit", `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, 429, llm.ClassRateLimited},
		{"overflow", `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 9 > 8"}}`, 400, llm.ClassOverflow},
		{"bad request", `{"type":"error","error":{"type":"invalid_request_error","message":"bad field"}}`, 400, llm.ClassFatal},
		{"auth", `{"type":"error","error":{"type":"authentication_error","message":"nope"}}`, 401, llm.ClassFatal},
		{"permission", `{"type":"error","error":{"type":"permission_error","message":"nope"}}`, 403, llm.ClassFatal},
		{"not found", `{"type":"error","error":{"type":"not_found_error","message":"no model"}}`, 404, llm.ClassFatal},
		{"api error", `{"type":"error","error":{"type":"api_error","message":"boom"}}`, 500, llm.ClassTransient},
		{"unknown type", `{"type":"error","error":{"type":"brand_new_error","message":"?"}}`, 500, llm.ClassTransient},
		{"no detail", `{"type":"error"}`, 500, llm.ClassTransient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			m := newModel(t, serve(events(
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
				tc.errEvent,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"never seen"}}`,
			)), anthropic.Config{})

			// Act
			sr, err := m.Stream(context.Background(), []*ai.Message{ai.UserMessage("hi")})
			if err != nil {
				t.Fatal(err)
			}
			chunks, err := recvAll(t, sr)

			// Assert
			var ae *anthropic.APIError
			if !errors.As(err, &ae) || ae.Status != tc.status {
				t.Fatalf("err %v, want APIError with status %d", err, tc.status)
			}
			if got := llm.Classify(err); got != tc.class {
				t.Errorf("classified %v, want %v", got, tc.class)
			}
			if len(chunks) != 1 || chunks[0].Content != "partial" {
				t.Errorf("chunks %+v: want only what came before the error", chunks)
			}
		})
	}
}

func TestStreamErrorEventCarriesTypeAndMessage(t *testing.T) {
	// Arrange
	m := newModel(t, serve(events(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)), anthropic.Config{})

	// Act
	_, err := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")})

	// Assert
	var ae *anthropic.APIError
	if !errors.As(err, &ae) || ae.Type != "overloaded_error" || ae.Message != "Overloaded" || ae.StatusCode() != 529 {
		t.Fatalf("err %#v", err)
	}
	if !strings.Contains(err.Error(), "529") || !strings.Contains(err.Error(), "Overloaded") {
		t.Errorf("error text %q lacks status or message", err)
	}
}

func TestStreamHandlesAnEventLargerThanTheDefaultScannerLine(t *testing.T) {
	// Arrange
	big := strings.Repeat("x", 300<<10)
	m := newModel(t, serve(events(
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+big+`"}}`,
		`{"type":"message_stop"}`,
	)), anthropic.Config{})

	// Act
	msg, err := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != big {
		t.Fatalf("content of %d bytes, want %d", len(msg.Content), len(big))
	}
}

func TestStreamEndsWithTheContextWhenItIsCancelledMidStream(t *testing.T) {
	// Arrange
	sent := make(chan struct{})
	m := newModel(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, events(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"one"}}`))
		w.(http.Flusher).Flush()
		close(sent)
		<-r.Context().Done()
	}, anthropic.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Act
	sr, err := m.Stream(ctx, []*ai.Message{ai.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	defer sr.Close()
	first, err := sr.Recv()
	if err != nil {
		t.Fatal(err)
	}
	<-sent
	cancel()
	_, err = sr.Recv()

	// Assert
	if first.Content != "one" {
		t.Errorf("first chunk %+v", first)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	if llm.Classify(err) != llm.ClassFatal {
		t.Errorf("cancellation classified %v, want fatal", llm.Classify(err))
	}
}

func TestClosingTheReaderReleasesTheConnection(t *testing.T) {
	// Arrange
	gone := make(chan struct{})
	started := make(chan struct{})
	m := newModel(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, events(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}`))
		w.(http.Flusher).Flush()
		close(started)
		// Keep producing until the client goes away.
		for r.Context().Err() == nil {
			if _, err := io.WriteString(w, events(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"b"}}`)); err != nil {
				break
			}
			w.(http.Flusher).Flush()
		}
		close(gone)
	}, anthropic.Config{})

	// Act
	sr, err := m.Stream(context.Background(), []*ai.Message{ai.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := sr.Recv(); err != nil {
		t.Fatal(err)
	}
	sr.Close()

	// Assert
	<-gone // the server only gets here once the client dropped the request
}

func TestErrorStatusesAreClassifiedAndKeepTheProviderMessage(t *testing.T) {
	long := strings.Repeat("y", 700)
	for _, tc := range []struct {
		name         string
		status       int
		body         string
		class        llm.ErrorClass
		typ, message string
	}{
		{"rate limited", 429, `{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit"}}`,
			llm.ClassRateLimited, "rate_limit_error", "Number of request tokens has exceeded your per-minute rate limit"},
		{"overloaded", 529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, llm.ClassTransient, "overloaded_error", "Overloaded"},
		{"api error", 500, `{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`, llm.ClassTransient, "api_error", "Internal server error"},
		{"bad gateway html", 502, "<html>bad gateway</html>\n", llm.ClassTransient, "", "<html>bad gateway</html>"},
		{"unauthorized", 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, llm.ClassFatal, "authentication_error", "invalid x-api-key"},
		{"not found", 404, `{"type":"error","error":{"type":"not_found_error","message":"model: nope"}}`, llm.ClassFatal, "not_found_error", "model: nope"},
		{"bad request", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: must be positive"}}`, llm.ClassFatal, "invalid_request_error", "max_tokens: must be positive"},
		{"empty body", 503, ``, llm.ClassTransient, "", ""},
		{"long plain body", 500, long, llm.ClassTransient, "", long[:500]},
		{"json without message", 400, `{"error":{"type":"invalid_request_error"}}`, llm.ClassFatal, "invalid_request_error", `{"error":{"type":"invalid_request_error"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			m := newModel(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}, anthropic.Config{})

			// Act
			sr, err := m.Stream(context.Background(), []*ai.Message{ai.UserMessage("hi")})

			// Assert
			if sr != nil {
				t.Fatal("a failed call returned a stream")
			}
			var ae *anthropic.APIError
			if !errors.As(err, &ae) {
				t.Fatalf("err %T %v, want *APIError", err, err)
			}
			if ae.Status != tc.status || ae.Type != tc.typ || ae.Message != tc.message {
				t.Errorf("APIError %+v, want status %d type %q message %q", ae, tc.status, tc.typ, tc.message)
			}
			if got := llm.Classify(err); got != tc.class {
				t.Errorf("classified %v, want %v", got, tc.class)
			}
		})
	}
}

func TestRefusedConnectionIsTransient(t *testing.T) {
	// Arrange: nothing listens on port 1.
	srv, err := anthropic.New(anthropic.Config{Model: "m", BaseURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	_, err = srv.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")})

	// Assert
	if err == nil || llm.Classify(err) != llm.ClassTransient {
		t.Fatalf("err %v classified %v, want transient", err, llm.Classify(err))
	}
}
