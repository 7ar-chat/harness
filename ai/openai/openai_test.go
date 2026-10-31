package openai_test

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
	"github.com/7ar-chat/harness/ai/openai"
)

// serve answers every request with body (JSON, or SSE when stream) and records the request body.
func serve(t *testing.T, status int, contentType, body string) (*openai.Model, *string) {
	t.Helper()
	var got string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	m, err := openai.New(openai.Config{BaseURL: ts.URL, APIKey: "k", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	return m, &got
}

func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: " + e + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func drain(t *testing.T, sr *ai.StreamReader[*ai.Message]) []*ai.Message {
	t.Helper()
	defer sr.Close()
	var out []*ai.Message
	for {
		m, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		out = append(out, m)
	}
}

func TestStreamedToolCallFragmentsMergeIntoOneCallWithUsage(t *testing.T) {
	// Arguments arrive in pieces under one index; the usage chunk (no choices) closes the stream.
	m, _ := serve(t, 200, "text/event-stream", sse(
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Let me look. "}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"pa"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a.go\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":9,"total_tokens":129,"prompt_tokens_details":{"cached_tokens":100},"completion_tokens_details":{"reasoning_tokens":4}}}`,
	))

	sr, err := m.Stream(context.Background(), []*ai.Message{ai.UserMessage("read it")})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ai.ConcatMessages(drain(t, sr))

	if err != nil {
		t.Fatal(err)
	}
	if out.Content != "Let me look. " || len(out.ToolCalls) != 1 {
		t.Fatalf("message %+v", out)
	}
	if c := out.ToolCalls[0]; c.ID != "call_1" || c.Function.Name != "read_file" || c.Function.Arguments != `{"path":"a.go"}` {
		t.Fatalf("call %+v", c)
	}
	u := out.ResponseMeta.Usage
	if out.ResponseMeta.FinishReason != "tool_calls" || u.PromptTokens != 120 || u.CompletionTokens != 9 ||
		u.PromptTokenDetails.CachedTokens != 100 || u.CompletionTokenDetails.ReasoningTokens != 4 {
		t.Fatalf("meta %+v usage %+v", out.ResponseMeta, u)
	}
}

func TestReasoningContentStreamsAlongsideTheAnswer(t *testing.T) {
	m, _ := serve(t, 200, "text/event-stream", sse(
		`{"choices":[{"index":0,"delta":{"reasoning_content":"think "}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"more","content":"42"},"finish_reason":"stop"}]}`,
	))
	sr, _ := m.Stream(context.Background(), []*ai.Message{ai.UserMessage("q")})

	out, err := ai.ConcatMessages(drain(t, sr))

	if err != nil || out.ReasoningContent != "think more" || out.Content != "42" {
		t.Fatalf("out %+v err %v", out, err)
	}
}

func TestAStreamThatEndsWithoutDoneOrFinishIsAnError(t *testing.T) {
	// A connection cut mid-answer must not look like a complete answer.
	m, _ := serve(t, 200, "text/event-stream", "data: "+`{"choices":[{"index":0,"delta":{"content":"par"}}]}`+"\n\n")
	sr, _ := m.Stream(context.Background(), []*ai.Message{ai.UserMessage("q")})
	defer sr.Close()

	var err error
	for err == nil {
		_, err = sr.Recv()
	}

	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err %v, want an unexpected EOF", err)
	}
}

func TestAnErrorEventInTheStreamIsReportedWithItsCode(t *testing.T) {
	m, _ := serve(t, 200, "text/event-stream", sse(`{"error":{"code":"context_length_exceeded","message":"too long"}}`))
	sr, _ := m.Stream(context.Background(), []*ai.Message{ai.UserMessage("q")})
	defer sr.Close()

	_, err := sr.Recv()

	var coded interface{ ErrorCode() string }
	if !errors.As(err, &coded) || coded.ErrorCode() != "context_length_exceeded" {
		t.Fatalf("err %v", err)
	}
}

func TestHTTPErrorsCarryStatusAndCode(t *testing.T) {
	m, _ := serve(t, 429, "application/json", `{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`)

	_, err := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("q")})

	var api *openai.APIError
	if !errors.As(err, &api) || api.StatusCode() != 429 || api.ErrorCode() != "rate_limit_exceeded" || api.Message != "slow down" {
		t.Fatalf("err %#v", err)
	}
}

func TestRequestBodyCarriesToolsChoiceOptionsAndImages(t *testing.T) {
	m, body := serve(t, 200, "application/json", `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	tm, _ := m.WithTools([]*ai.ToolInfo{{Name: "read_file", Desc: "reads", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}})
	msgs := []*ai.Message{
		ai.SystemMessage("sys"),
		{Role: ai.RoleUser, Parts: []ai.Part{
			{Type: ai.PartText, Text: "look"},
			{Type: ai.PartImage, Image: &ai.Image{Base64Data: "QUJD", MIMEType: "image/png"}},
		}},
		ai.AssistantMessage("", []ai.ToolCall{{ID: "c1", Function: ai.FunctionCall{Name: "read_file", Arguments: `{"path":"x"}`}}}),
		ai.ToolMessage("contents", "c1", ai.WithToolName("read_file")),
	}

	_, err := tm.Generate(context.Background(), msgs,
		ai.WithToolChoice(ai.ToolChoiceForced, "read_file"), ai.WithReasoningEffort("low"), ai.WithPromptCacheKey("t-1"), ai.WithMaxTokens(77))

	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
			ToolCalls  []struct {
				Type string `json:"type"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools      []struct{ Type string } `json:"tools"`
		ToolChoice struct {
			Function struct{ Name string } `json:"function"`
		} `json:"tool_choice"`
		ReasoningEffort string `json:"reasoning_effort"`
		PromptCacheKey  string `json:"prompt_cache_key"`
		MaxTokens       int    `json:"max_tokens"`
	}
	if err := json.Unmarshal([]byte(*body), &req); err != nil {
		t.Fatalf("%v: %s", err, *body)
	}
	if req.Model != "m" || req.Stream || len(req.Tools) != 1 || req.Tools[0].Type != "function" || req.ToolChoice.Function.Name != "read_file" ||
		req.ReasoningEffort != "low" || req.PromptCacheKey != "t-1" || req.MaxTokens != 77 {
		t.Fatalf("request %s", *body)
	}
	if !strings.Contains(string(req.Messages[1].Content), `"url":"data:image/png;base64,QUJD"`) || !strings.Contains(string(req.Messages[1].Content), `"type":"text"`) {
		t.Fatalf("image message %s", req.Messages[1].Content)
	}
	if req.Messages[2].ToolCalls[0].Type != "function" || req.Messages[3].Role != "tool" || req.Messages[3].ToolCallID != "c1" {
		t.Fatalf("tool messages %+v", req.Messages[2:])
	}
}

func TestToolChoiceMapping(t *testing.T) {
	for _, c := range []struct {
		choice ai.ToolChoice
		names  []string
		want   string
	}{
		{ai.ToolChoiceForbidden, nil, `"tool_choice":"none"`},
		{ai.ToolChoiceForced, nil, `"tool_choice":"required"`},
		{ai.ToolChoiceAllowed, nil, `"tool_choice":"auto"`},
	} {
		m, body := serve(t, 200, "application/json", `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
		tm, _ := m.WithTools([]*ai.ToolInfo{{Name: "t"}})

		_, err := tm.Generate(context.Background(), []*ai.Message{ai.UserMessage("q")}, ai.WithToolChoice(c.choice, c.names...))

		if err != nil || !strings.Contains(*body, c.want) {
			t.Errorf("%s: err %v body %s", c.choice, err, *body)
		}
	}
}

func TestNoToolChoiceIsSentWithoutTools(t *testing.T) {
	// A forbidden choice on a model with no tools must not reach providers that reject tool_choice without tools.
	m, body := serve(t, 200, "application/json", `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)

	_, err := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("q")}, ai.WithToolChoice(ai.ToolChoiceForbidden))

	if err != nil || strings.Contains(*body, "tool_choice") {
		t.Fatalf("err %v body %s", err, *body)
	}
}
