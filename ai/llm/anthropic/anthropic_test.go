package anthropic_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm"
	"github.com/7ar-chat/harness/ai/llm/anthropic"
)

const sse = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":12,"cache_read_input_tokens":900,"cache_creation_input_tokens":40,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me look."}}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"/workspace/a.go\"}"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":33}}

event: message_stop
data: {"type":"message_stop"}

`

func newModel(t *testing.T, handler http.HandlerFunc, cfg anthropic.Config) *anthropic.Model {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg.BaseURL, cfg.APIKey, cfg.Model = srv.URL, "k", "claude-test"
	m, err := anthropic.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestStreamAssemblesTextToolCallsAndCacheUsage(t *testing.T) {
	// Arrange
	m := newModel(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "k" || r.URL.Path != "/v1/messages" {
			t.Errorf("bad request %s %v", r.URL.Path, r.Header)
		}
		io.WriteString(w, sse)
	}, anthropic.Config{})

	// Act
	msg, err := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "Let me look." || len(msg.ToolCalls) != 1 {
		t.Fatalf("message %+v", msg)
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Function.Name != "read_file" || tc.Function.Arguments != `{"path":"/workspace/a.go"}` {
		t.Fatalf("tool call %+v", tc)
	}
	u := msg.ResponseMeta.Usage
	if msg.ResponseMeta.FinishReason != "tool_calls" || u.PromptTokens != 952 || u.PromptTokenDetails.CachedTokens != 900 ||
		u.PromptTokenDetails.CacheWriteTokens != 40 || u.CompletionTokens != 33 {
		t.Fatalf("meta %+v usage %+v", msg.ResponseMeta, u)
	}
}

func TestRequestMarksCacheBreakpointsAndMergesToolResults(t *testing.T) {
	// Arrange
	var got map[string]any
	m := newModel(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}, anthropic.Config{})
	tools, _ := m.WithTools([]*ai.ToolInfo{{Name: "a", Desc: "A"}, {Name: "b", Desc: "B"}})
	call := func(i int, id string) ai.ToolCall {
		return ai.ToolCall{Index: &i, ID: id, Type: "function", Function: ai.FunctionCall{Name: "a", Arguments: `{"x":1}`}}
	}

	// Act
	_, err := tools.Generate(context.Background(), []*ai.Message{
		ai.SystemMessage("be brief"),
		ai.UserMessage("go"),
		{Role: ai.RoleAssistant, Content: "ok", ToolCalls: []ai.ToolCall{call(0, "t1"), call(1, "t2")}},
		ai.ToolMessage("r1", "t1"),
		ai.ToolMessage("r2", "t2"),
		ai.UserMessage("[steer] and also this"),
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	s := string(b)
	norm := func(frag string) string { // the request is decoded into maps, so keys are sorted
		var v any
		if err := json.Unmarshal([]byte(frag), &v); err != nil {
			t.Fatal(err)
		}
		out, _ := json.Marshal(v)
		return string(out)
	}
	for _, want := range []string{
		`[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}]`,
		`{"name":"b","description":"B","input_schema":{"properties":{},"type":"object"},"cache_control":{"type":"ephemeral"}}`,
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"r1"},{"type":"tool_result","tool_use_id":"t2","content":"r2"},{"type":"text","text":"[steer] and also this","cache_control":{"type":"ephemeral"}}]}`,
	} {
		want = norm(want)
		if !strings.Contains(s, want) {
			t.Errorf("request lacks %s\n%s", want, s)
		}
	}
	if strings.Count(s, "cache_control") != 3 {
		t.Errorf("want 3 breakpoints:\n%s", s)
	}
}

func TestErrorsAreClassifiedLikeTheOtherProviders(t *testing.T) {
	// Arrange
	status := 400
	body := `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 250000 tokens > 200000 maximum"}}`
	m := newModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}, anthropic.Config{})

	// Act
	_, errOverflow := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")})
	status, body = 529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
	_, errBusy := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")})

	// Assert
	var ae *anthropic.APIError
	if !errors.As(errOverflow, &ae) || llm.Classify(errOverflow) != llm.ClassOverflow {
		t.Errorf("overflow classified %v (%v)", llm.Classify(errOverflow), errOverflow)
	}
	if llm.Classify(errBusy) != llm.ClassTransient {
		t.Errorf("overloaded classified %v", llm.Classify(errBusy))
	}
}

func TestNoCacheOmitsBreakpoints(t *testing.T) {
	// Arrange
	var raw []byte
	m := newModel(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}, anthropic.Config{NoCache: true})

	// Act
	_, _ = m.Generate(context.Background(), []*ai.Message{ai.SystemMessage("s"), ai.UserMessage("u")})

	// Assert
	if strings.Contains(string(raw), "cache_control") {
		t.Fatalf("breakpoints sent: %s", raw)
	}
}
