package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestNewRequiresAModelAndDefaultsToTheOpenAIEndpoint(t *testing.T) {
	// A model id is mandatory; without a BaseURL, calls go to api.openai.com/v1 over the default client, with no Authorization header when there is no key.
	// Arrange
	var url string
	var auth []string
	hc := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		url, auth = r.URL.String(), r.Header.Values("Authorization")
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}]}`))}, nil
	})}

	// Act
	none, errNone := openai.New(openai.Config{})
	m, err := openai.New(openai.Config{Model: "m", HTTPClient: hc})
	if err != nil {
		t.Fatal(err)
	}
	_, errCall := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("q")})

	// Assert
	if errNone == nil || none != nil {
		t.Errorf("New without a model: %v %v", none, errNone)
	}
	if errCall != nil || url != "https://api.openai.com/v1/chat/completions" || len(auth) != 0 {
		t.Errorf("err %v url %q auth %v", errCall, url, auth)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRequestHeadersAndBaseURLSlashes(t *testing.T) {
	// The bearer key, content type and per-call Accept header are sent; a trailing slash on BaseURL does not double up.
	// Arrange
	var path string
	var hdr http.Header
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, hdr = r.URL.Path, r.Header.Clone()
		if r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(sse(`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`)))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(ts.Close)
	m, _ := openai.New(openai.Config{BaseURL: ts.URL + "/v1///", APIKey: "secret", Model: "m"})

	// Act
	_, errG := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("q")})
	genAccept := hdr.Get("Accept")
	sr, errS := m.Stream(context.Background(), []*ai.Message{ai.UserMessage("q")})
	if errS == nil {
		drain(t, sr)
	}

	// Assert
	if errG != nil || errS != nil {
		t.Fatal(errG, errS)
	}
	if path != "/v1/chat/completions" || hdr.Get("Authorization") != "Bearer secret" || hdr.Get("Content-Type") != "application/json" ||
		genAccept != "application/json" || hdr.Get("Accept") != "text/event-stream" {
		t.Fatalf("path %q headers %v (generate accept %q)", path, hdr, genAccept)
	}
}

func TestStreamRequestAsksForUsageAndGenerateDoesNot(t *testing.T) {
	// Streaming includes the usage chunk (stream_options.include_usage); the non-streamed call neither streams nor sets options.
	// Arrange
	m, body := serve(t, 200, "text/event-stream", sse(`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`))
	mj, bodyJSON := serve(t, 200, "application/json", `{"choices":[{"message":{"content":"x"}}]}`)

	// Act
	sr, err := m.Stream(context.Background(), []*ai.Message{ai.UserMessage("q")})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, sr)
	_, errJSON := mj.Generate(context.Background(), []*ai.Message{ai.UserMessage("q")})

	// Assert
	if errJSON != nil {
		t.Fatal(errJSON)
	}
	if !strings.Contains(*body, `"stream":true`) || !strings.Contains(*body, `"stream_options":{"include_usage":true}`) {
		t.Errorf("stream body %s", *body)
	}
	if strings.Contains(*bodyJSON, "stream") {
		t.Errorf("generate body %s", *bodyJSON)
	}
}

func TestToolsAreOfferedWithDefaultSchemaAndWithoutNilEntries(t *testing.T) {
	// A tool without a parameter schema is offered with an empty object schema (providers reject a missing one); nil entries are skipped; WithTools leaves the receiver without tools.
	// Arrange
	m, body := serve(t, 200, "application/json", `{"choices":[{"message":{"content":"x"}}]}`)
	tm, err := m.WithTools([]*ai.ToolInfo{nil, {Name: "now", Desc: "time"}})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	_, errT := tm.Generate(context.Background(), []*ai.Message{ai.UserMessage("q")})
	withTools := *body
	_, errM := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("q")})

	// Assert
	if errT != nil || errM != nil {
		t.Fatal(errT, errM)
	}
	var req struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				Parameters  map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(withTools), &req); err != nil || len(req.Tools) != 1 {
		t.Fatalf("%v: %s", err, withTools)
	}
	f := req.Tools[0].Function
	if req.Tools[0].Type != "function" || f.Name != "now" || f.Description != "time" || f.Parameters["type"] != "object" {
		t.Errorf("tool %+v", req.Tools[0])
	}
	if strings.Contains(*body, `"tools"`) {
		t.Errorf("receiver sent tools: %s", *body)
	}
}

func TestForcedChoiceNamingOneToolPinsThatFunction(t *testing.T) {
	// A forced choice with exactly one allowed name pins that function; with several names the model may pick any tool.
	// Arrange
	m, body := serve(t, 200, "application/json", `{"choices":[{"message":{"content":"x"}}]}`)
	tm, _ := m.WithTools([]*ai.ToolInfo{{Name: "a"}, {Name: "b"}})

	// Act
	_, _ = tm.Generate(context.Background(), []*ai.Message{ai.UserMessage("q")}, ai.WithToolChoice(ai.ToolChoiceForced, "b"))
	one := *body
	_, _ = tm.Generate(context.Background(), []*ai.Message{ai.UserMessage("q")}, ai.WithToolChoice(ai.ToolChoiceForced, "a", "b"))

	// Assert
	if !strings.Contains(one, `"tool_choice":{"function":{"name":"b"},"type":"function"}`) {
		t.Errorf("single: %s", one)
	}
	if !strings.Contains(*body, `"tool_choice":"required"`) {
		t.Errorf("several: %s", *body)
	}
}

func TestSamplingOptionsAreSentAndOmittedWhenUnset(t *testing.T) {
	// Temperature and top_p reach the body only when the caller set them (a zero temperature is a real value).
	// Arrange
	m, body := serve(t, 200, "application/json", `{"choices":[{"message":{"content":"x"}}]}`)

	// Act
	_, _ = m.Generate(context.Background(), []*ai.Message{ai.UserMessage("q")}, ai.WithTemperature(0), ai.WithTopP(0.5))
	set := *body
	_, _ = m.Generate(context.Background(), []*ai.Message{ai.UserMessage("q")})

	// Assert
	if !strings.Contains(set, `"temperature":0`) || !strings.Contains(set, `"top_p":0.5`) {
		t.Errorf("set: %s", set)
	}
	if strings.Contains(*body, "temperature") || strings.Contains(*body, "top_p") || strings.Contains(*body, "max_tokens") {
		t.Errorf("unset: %s", *body)
	}
}

func TestMessageConversionKeepsNamesReasoningAndToolCallDefaults(t *testing.T) {
	// Assistant reasoning and message names are sent back, a tool call without a type is a function, image parts without a usable source are dropped, an image keeps its detail and URL, and only user messages use parts.
	// Arrange
	m, body := serve(t, 200, "application/json", `{"choices":[{"message":{"content":"x"}}]}`)
	assistant := ai.AssistantMessage("", []ai.ToolCall{{ID: "c1", Function: ai.FunctionCall{Name: "f", Arguments: "{}"}}})
	assistant.ReasoningContent = "thinking"
	named := ai.UserMessage("hi")
	named.Name = "alice"
	images := &ai.Message{Role: ai.RoleUser, Parts: []ai.Part{
		{Type: ai.PartImage, Image: &ai.Image{URL: "https://x/y.png", Detail: "high"}},
		{Type: ai.PartImage, Image: &ai.Image{Base64Data: "QQ==", MIMEType: "image/webp"}},
		{Type: ai.PartImage, Image: &ai.Image{}},
		{Type: ai.PartImage},
		{Type: "video"},
	}}
	sysWithParts := &ai.Message{Role: ai.RoleSystem, Content: "sys", Parts: []ai.Part{{Type: ai.PartText, Text: "ignored"}}}

	// Act
	_, err := m.Generate(context.Background(), []*ai.Message{sysWithParts, nil, named, images, assistant})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(*body), &req); err != nil || len(req.Messages) != 4 {
		t.Fatalf("%v: %s", err, *body)
	}
	if req.Messages[0]["content"] != "sys" || req.Messages[1]["name"] != "alice" || req.Messages[1]["content"] != "hi" {
		t.Errorf("system/user: %v %v", req.Messages[0], req.Messages[1])
	}
	parts, _ := req.Messages[2]["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("image parts %v, want the 2 usable ones", req.Messages[2]["content"])
	}
	first := parts[0].(map[string]any)["image_url"].(map[string]any)
	second := parts[1].(map[string]any)["image_url"].(map[string]any)
	if first["url"] != "https://x/y.png" || first["detail"] != "high" || second["url"] != "data:image/webp;base64,QQ==" {
		t.Errorf("images %v %v", first, second)
	}
	if req.Messages[3]["reasoning_content"] != "thinking" || req.Messages[3]["tool_calls"].([]any)[0].(map[string]any)["type"] != "function" {
		t.Errorf("assistant %v", req.Messages[3])
	}
	if strings.Contains(*body, "ignored") {
		t.Error("parts of a non-user message were sent")
	}
}

func TestImageWithoutMIMETypeIsSentAsPNG(t *testing.T) {
	// Base64 images without a media type default to PNG so the data URL stays valid.
	// Arrange
	m, body := serve(t, 200, "application/json", `{"choices":[{"message":{"content":"x"}}]}`)
	msg := &ai.Message{Role: ai.RoleUser, Parts: []ai.Part{{Type: ai.PartImage, Image: &ai.Image{Base64Data: "QUJD"}}}}

	// Act
	_, err := m.Generate(context.Background(), []*ai.Message{msg})

	// Assert
	if err != nil || !strings.Contains(*body, `data:image/png;base64,QUJD`) {
		t.Fatalf("err %v body %s", err, *body)
	}
}

func TestGenerateParsesTheAnswer(t *testing.T) {
	// A non-streamed answer yields its first choice with text, reasoning, tool calls, finish reason and every usage figure; the role defaults to assistant.
	// Arrange
	m, _ := serve(t, 200, "application/json", `{"choices":[
		{"message":{"content":"hi","reasoning_content":"hmm","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"},
		{"message":{"content":"second"}}],
		"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14,"prompt_tokens_details":{"cached_tokens":6},"completion_tokens_details":{"reasoning_tokens":2}}}`)

	// Act
	out, err := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("q")})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	u := out.ResponseMeta.Usage
	if out.Role != ai.RoleAssistant || out.Content != "hi" || out.ReasoningContent != "hmm" || out.ResponseMeta.FinishReason != "tool_calls" ||
		len(out.ToolCalls) != 1 || out.ToolCalls[0].ID != "c1" || out.ToolCalls[0].Function.Arguments != `{"a":1}` ||
		u.PromptTokens != 10 || u.CompletionTokens != 4 || u.TotalTokens != 14 || u.PromptTokenDetails.CachedTokens != 6 || u.CompletionTokenDetails.ReasoningTokens != 2 {
		t.Fatalf("out %+v usage %+v", out, u)
	}
}

func TestGenerateWithoutUsageReportsNone(t *testing.T) {
	// An answer with no usage object has nil usage (not zeros), and a role sent by the gateway is kept.
	// Arrange
	m, _ := serve(t, 200, "application/json", `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"length"}]}`)

	// Act
	out, err := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("q")})

	// Assert
	if err != nil || out.ResponseMeta.Usage != nil || out.ResponseMeta.FinishReason != "length" {
		t.Fatalf("err %v out %+v", err, out.ResponseMeta)
	}
}

func TestGenerateRejectsUnusableAnswers(t *testing.T) {
	// A 200 answer that is not JSON, has no choices, or carries an error object is an error, with the provider's code when it gave one.
	// Arrange
	cases := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"not json", `<html>gateway</html>`, ""},
		{"no choices", `{"choices":[]}`, ""},
		{"error object", `{"error":{"code":"insufficient_quota","type":"billing","message":"pay up"}}`, "insufficient_quota"},
		{"numeric code", `{"error":{"code":402,"message":"pay up"}}`, "402"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := serve(t, 200, "application/json", tc.body)

			// Act
			out, err := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("q")})

			// Assert
			if err == nil || out != nil {
				t.Fatalf("out %+v err %v, want an error", out, err)
			}
			var api *openai.APIError
			if tc.wantCode != "" && (!errors.As(err, &api) || api.ErrorCode() != tc.wantCode || api.StatusCode() != 0) {
				t.Fatalf("err %#v, want code %q and no status", err, tc.wantCode)
			}
		})
	}
}

func TestHTTPErrorBodiesAreReadWhateverTheirShape(t *testing.T) {
	// Status errors carry the provider's code/type/message, a numeric code as text, and for non-JSON bodies the (bounded) raw text.
	// Arrange
	cases := []struct {
		name         string
		status       int
		body         string
		code, typ, m string
	}{
		{"typed", 400, `{"error":{"code":"context_length_exceeded","type":"invalid_request_error","message":"too long"}}`, "context_length_exceeded", "invalid_request_error", "too long"},
		{"numeric code", 429, `{"error":{"code":429,"message":"slow"}}`, "429", "", "slow"},
		{"null code", 400, `{"error":{"code":null,"message":"bad"}}`, "", "", "bad"},
		{"code only", 503, `{"error":{"code":"overloaded"}}`, "overloaded", "", ""},
		{"html", 502, "  <html>bad gateway</html>\n", "", "", "<html>bad gateway</html>"},
		{"json without error", 500, `{"detail":"x"}`, "", "", `{"detail":"x"}`},
		{"huge", 502, strings.Repeat("y", 5000), "", "", strings.Repeat("y", 2000)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := serve(t, tc.status, "application/json", tc.body)

			// Act
			_, err := m.Stream(context.Background(), []*ai.Message{ai.UserMessage("q")})

			// Assert
			var api *openai.APIError
			if !errors.As(err, &api) || api.Status != tc.status || api.Code != tc.code || api.Type != tc.typ || api.Message != tc.m {
				t.Fatalf("err %#v", err)
			}
			if !strings.Contains(api.Error(), tc.m) || !strings.Contains(api.Error(), fmt.Sprint(tc.status)) {
				t.Errorf("Error() = %q", api.Error())
			}
		})
	}
}

func TestStreamedChunksIgnoreOtherChoicesAndEmptyEvents(t *testing.T) {
	// Only choice 0 is followed; events with nothing to say (role-only deltas, keep-alive data lines) produce no chunk; non-data lines are skipped; text before the finish event is kept in order.
	// Arrange
	body := ": comment\n" +
		"event: ping\n" +
		"data:\n\n" +
		"data: " + `{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}` + "\n\n" +
		"data: " + `{"choices":[{"index":1,"delta":{"content":"other choice"}},{"index":0,"delta":{"content":"A"}}]}` + "\n\n" +
		"data:" + `{"choices":[{"index":0,"delta":{"content":"B"},"finish_reason":"stop"}]}` + "\r\n\r\n"
	m, _ := serve(t, 200, "text/event-stream", body)
	sr, _ := m.Stream(context.Background(), []*ai.Message{ai.UserMessage("q")})

	// Act
	chunks := drain(t, sr)

	// Assert
	out, err := ai.ConcatMessages(chunks)
	if err != nil || out.Content != "AB" || len(chunks) != 2 || out.ResponseMeta.FinishReason != "stop" {
		t.Fatalf("err %v chunks %d out %+v", err, len(chunks), out)
	}
}

func TestStreamEndingAfterFinishReasonWithoutDoneIsComplete(t *testing.T) {
	// Some gateways omit [DONE]; a stream that reached its finish_reason is a complete answer.
	// Arrange
	m, _ := serve(t, 200, "text/event-stream", "data: "+`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
	sr, _ := m.Stream(context.Background(), []*ai.Message{ai.UserMessage("q")})

	// Act
	out, err := ai.ConcatMessages(drain(t, sr))

	// Assert
	if err != nil || out.Content != "ok" {
		t.Fatalf("err %v out %+v", err, out)
	}
}

func TestMalformedEventFailsTheStream(t *testing.T) {
	// A data line that is not JSON ends the stream with an error after the text already delivered.
	// Arrange
	m, _ := serve(t, 200, "text/event-stream", "data: "+`{"choices":[{"index":0,"delta":{"content":"ok"}}]}`+"\n\ndata: {oops\n\n")
	sr, _ := m.Stream(context.Background(), []*ai.Message{ai.UserMessage("q")})
	defer sr.Close()

	// Act
	first, err1 := sr.Recv()
	_, err2 := sr.Recv()

	// Assert
	var syn *json.SyntaxError
	if err1 != nil || first.Content != "ok" || !errors.As(err2, &syn) {
		t.Fatalf("first %+v err1 %v err2 %v", first, err1, err2)
	}
}

func TestErrorEventWithNumericCodeAndTypeIsReported(t *testing.T) {
	// A mid-stream error object keeps its type and a numeric code as text; it has no HTTP status.
	// Arrange
	m, _ := serve(t, 200, "text/event-stream", "data: "+`{"error":{"code":500,"type":"server_error","message":"upstream died"}}`+"\n\n")
	sr, _ := m.Stream(context.Background(), []*ai.Message{ai.UserMessage("q")})
	defer sr.Close()

	// Act
	_, err := sr.Recv()

	// Assert
	var api *openai.APIError
	if !errors.As(err, &api) || api.Code != "500" || api.Type != "server_error" || api.Message != "upstream died" || api.Status != 0 {
		t.Fatalf("err %#v", err)
	}
}

func TestCancelledContextStopsAStreamBetweenEvents(t *testing.T) {
	// When the caller cancels while events are still arriving, the stream ends with the context error instead of a truncated answer.
	// Arrange
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"choices":[{"index":0,"delta":{"content":"one"}}]}` + "\n\n"))
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte("data: " + `{"choices":[{"index":0,"delta":{"content":"two"}}]}` + "\n\n"))
	}))
	t.Cleanup(ts.Close)
	m, _ := openai.New(openai.Config{BaseURL: ts.URL, Model: "m"})
	sr, err := m.Stream(ctx, []*ai.Message{ai.UserMessage("q")})
	if err != nil {
		t.Fatal(err)
	}
	defer sr.Close()
	first, err := sr.Recv()
	if err != nil || first.Content != "one" {
		t.Fatalf("first %+v err %v", first, err)
	}

	// Act
	cancel()
	close(release)
	var last error
	for last == nil {
		_, last = sr.Recv()
	}

	// Assert
	if !errors.Is(last, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", last)
	}
}

func TestOversizedEventLineIsAStreamError(t *testing.T) {
	// A single event bigger than the 8 MiB line limit fails the stream with the scanner's error rather than hanging or truncating silently.
	// Arrange
	huge := "data: " + strings.Repeat("a", 9<<20) + "\n\n"
	m, _ := serve(t, 200, "text/event-stream", huge)
	sr, _ := m.Stream(context.Background(), []*ai.Message{ai.UserMessage("q")})
	defer sr.Close()

	// Act
	_, err := sr.Recv()

	// Assert
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("err %v, want a scan error", err)
	}
}

func TestTransportFailureIsReturnedFromStreamAndGenerate(t *testing.T) {
	// A connection failure surfaces from both entry points before any chunk.
	// Arrange
	ts := httptest.NewServer(http.NotFoundHandler())
	url := ts.URL
	ts.Close()
	m, _ := openai.New(openai.Config{BaseURL: url, Model: "m"})

	// Act
	sr, errS := m.Stream(context.Background(), []*ai.Message{ai.UserMessage("q")})
	out, errG := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("q")})

	// Assert
	if errS == nil || sr != nil || errG == nil || out != nil {
		t.Fatal(errS, errG)
	}
}

func TestBadBaseURLIsAnError(t *testing.T) {
	// A malformed endpoint fails the call instead of panicking.
	// Arrange
	m, _ := openai.New(openai.Config{BaseURL: "http://bad host\x7f", Model: "m"})

	// Act
	_, errS := m.Stream(context.Background(), []*ai.Message{ai.UserMessage("q")})
	_, errG := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("q")})

	// Assert
	if errS == nil || errG == nil {
		t.Fatal(errS, errG)
	}
}

func TestGenerateReportsABodyThatBreaksMidRead(t *testing.T) {
	// A connection torn down while the answer body is read is an error, never a half-parsed answer.
	// Arrange
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte(`{"choices":[`))
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(ts.Close)
	m, _ := openai.New(openai.Config{BaseURL: ts.URL, Model: "m"})

	// Act
	out, err := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("q")})

	// Assert
	if err == nil || out != nil {
		t.Fatalf("out %+v err %v", out, err)
	}
}
