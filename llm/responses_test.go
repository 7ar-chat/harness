package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/llm"
	"github.com/GhaziBenDahmane/harness/llm/responses"
)

type responsesRequest struct {
	path, authorization string
	body                map[string]any
}

// responsesServer serves the SSE fixture (or the handler's own answer) and records the request.
func responsesServer(t *testing.T, fixture string, handler http.HandlerFunc) (*httptest.Server, *[]responsesRequest) {
	t.Helper()
	var got []responsesRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		req := responsesRequest{path: r.URL.Path, authorization: r.Header.Get("Authorization")}
		_ = json.Unmarshal(b, &req.body)
		got = append(got, req)
		if handler != nil {
			handler(w, r)
			return
		}
		data, err := os.ReadFile("testdata/responses/" + fixture)
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(data)
	}))
	t.Cleanup(ts.Close)
	return ts, &got
}

func responsesConfig(url string, models ...string) *config.Config {
	cfg := openaiConfig(url, 10, models...)
	cfg.LLMResponsesModels = models
	return cfg
}

func drain(t *testing.T, sr *schema.StreamReader[*schema.Message]) ([]*schema.Message, error) {
	t.Helper()
	defer sr.Close()
	var chunks []*schema.Message
	for {
		c, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			return chunks, nil
		}
		if err != nil {
			return chunks, err
		}
		chunks = append(chunks, c)
	}
}

func TestResponsesModelStreamsToolCalls(t *testing.T) {
	// A model in LLM_RESPONSES_MODELS calls {LLM_BASE_URL}/responses with the bearer key, and the recorded stream becomes two parallel tool calls, the usage and the reasoning item of output_item.done.
	// Arrange
	ts, got := responsesServer(t, "tool_calls.sse", nil)
	m, err := llm.New(responsesConfig(ts.URL, "gpt-5.6-terra")).Model(context.Background(), "gpt-5.6-terra")
	if err != nil {
		t.Fatal(err)
	}

	// Act
	out, err := m.Generate(context.Background(), []*schema.Message{schema.SystemMessage("sys"), schema.UserMessage("What is (17*23)+(91*7)?")},
		append(llm.WithReasoningEffort("medium"), llm.WithPromptCacheKey("thread-1")...)...)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	req := (*got)[0]
	if req.path != "/v1/responses" || req.authorization != "Bearer test-key" {
		t.Fatalf("request %+v", req)
	}
	b := req.body
	reasoning, _ := b["reasoning"].(map[string]any)
	if b["store"] != false || b["stream"] != true || b["model"] != "gpt-5.6-terra" || b["prompt_cache_key"] != "thread-1" ||
		reasoning["effort"] != "medium" || len(b["include"].([]any)) != 1 || b["include"].([]any)[0] != "reasoning.encrypted_content" {
		t.Fatalf("body %v", b)
	}
	if len(out.ToolCalls) != 2 || out.ToolCalls[0].ID != "call_s3CUVXHm99IsanXFw9WjORMQ" || out.ToolCalls[0].Function.Arguments != `{"expr":"17*23"}` ||
		out.ToolCalls[1].ID != "call_JiqQXwp1nLnq1PISu51ccqcD" || out.ToolCalls[1].Function.Name != "calc" || *out.ToolCalls[1].Index != 1 {
		t.Fatalf("tool calls %+v", out.ToolCalls)
	}
	u := out.ResponseMeta.Usage
	if out.ResponseMeta.FinishReason != "tool_calls" || u.PromptTokens != 82 || u.CompletionTokens != 65 || u.TotalTokens != 147 ||
		u.CompletionTokensDetails.ReasoningTokens != 13 {
		t.Fatalf("meta %+v usage %+v", out.ResponseMeta, u)
	}
	items := responses.ItemsOf(out)
	if len(items) != 1 || items[0].ID != "rs_01a66e2731f33e16016ac64c71b38c81949b2dae6babea3b59" ||
		items[0].EncryptedContent != "gAAAAA-fixture-done-1208" || items[0].Model != "gpt-5.6-terra" {
		t.Fatalf("reasoning items %+v", items)
	}
}

func TestResponsesModelStreamsText(t *testing.T) {
	// output_text deltas stream as content chunks and response.completed ends the answer with finish_reason stop.
	// Arrange
	ts, _ := responsesServer(t, "text.sse", nil)
	m, _ := llm.New(responsesConfig(ts.URL, "gpt-5.6-terra")).Model(context.Background(), "gpt-5.6-terra")

	// Act
	sr, err := m.Stream(context.Background(), []*schema.Message{schema.UserMessage("Say hello in 3 words.")})
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := drain(t, sr)

	// Assert
	var text strings.Builder
	for _, c := range chunks {
		text.WriteString(c.Content)
	}
	last := chunks[len(chunks)-1]
	if err != nil || text.String() != "Hello, nice to meet." || len(chunks) != 7 || last.ResponseMeta.FinishReason != "stop" ||
		last.ResponseMeta.Usage.PromptTokens != 13 {
		t.Fatalf("err %v text %q chunks %d last %+v", err, text.String(), len(chunks), last.ResponseMeta)
	}
}

func TestResponsesIncompleteAnswerFinishesWithLength(t *testing.T) {
	// response.incomplete (max_output_tokens) maps to finish_reason length, with its usage.
	// Arrange
	ts, _ := responsesServer(t, "incomplete.sse", nil)
	m, _ := llm.New(responsesConfig(ts.URL, "gpt-5.6-terra")).Model(context.Background(), "gpt-5.6-terra")

	// Act
	out, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("Write a poem.")})

	// Assert
	if err != nil || out.ResponseMeta.FinishReason != "length" || out.ResponseMeta.Usage.CompletionTokensDetails.ReasoningTokens != 16 {
		t.Fatalf("err %v meta %+v", err, out.ResponseMeta)
	}
}

func TestResponsesHistoryCarriesReasoningBeforeItsCalls(t *testing.T) {
	// An assistant message is sent as its reasoning items (of the same model only), its text and its function calls, followed by the function_call_output items.
	// Arrange
	ts, got := responsesServer(t, "text.sse", nil)
	m, _ := llm.New(responsesConfig(ts.URL, "gpt-5.6-terra")).Model(context.Background(), "gpt-5.6-terra")
	am := schema.AssistantMessage("Let me compute.", []schema.ToolCall{{ID: "c1", Type: "function", Function: schema.FunctionCall{Name: "calc", Arguments: `{"expr":"1+1"}`}}})
	responses.SetItems(am, []responses.ReasoningItem{
		{ID: "rs_1", EncryptedContent: "enc-1", Model: "gpt-5.6-terra"},
		{ID: "rs_other", EncryptedContent: "enc-2", Model: "gpt-5.6-sol"},
	})
	history := []*schema.Message{schema.SystemMessage("sys"), schema.UserMessage("1+1?"), am, schema.ToolMessage("2", "c1")}

	// Act
	_, err := m.Generate(context.Background(), history)

	// Assert
	input, _ := (*got)[0].body["input"].([]any)
	var kinds []string
	for _, raw := range input {
		it := raw.(map[string]any)
		kind := it["type"].(string)
		if r, ok := it["role"].(string); ok {
			kind += ":" + r
		}
		if id, ok := it["id"].(string); ok {
			kind += ":" + id + ":" + it["encrypted_content"].(string)
		}
		if id, ok := it["call_id"].(string); ok {
			kind += ":" + id
		}
		kinds = append(kinds, kind)
	}
	want := "message:system message:user reasoning:rs_1:enc-1 message:assistant function_call:c1 function_call_output:c1"
	if err != nil || strings.Join(kinds, " ") != want {
		t.Fatalf("err %v input %v", err, kinds)
	}
}

func TestResponsesErrorsAreClassifiedLikeChatCompletions(t *testing.T) {
	// HTTP errors and stream error events of the Responses API get the classes of the agent's retry policy, and Retry-After reaches CallInfo.
	// Arrange
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    llm.ErrorClass
		after   time.Duration
	}{
		{"429", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"429","message":"Requests to the Responses_Create Operation have exceeded token rate limit"}}`))
		}, llm.ClassRateLimited, 7 * time.Second},
		{"overflow", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"context_length_exceeded","type":"invalid_request_error","message":"Your input exceeds the context window of this model."}}`))
		}, llm.ClassOverflow, 0},
		{"bad request", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":null,"type":"invalid_request_error","message":"No tool output found for function call c1."}}`))
		}, llm.ClassFatal, 0},
		{"error event", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n" +
				"event: error\ndata: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\",\"param\":null}\n\n"))
		}, llm.ClassRateLimited, 0},
		{"failed", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"The server had an error\"}}}\n\n"))
		}, llm.ClassTransient, 0},
		{"cut", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"delta\":\"par\"}\n\n"))
		}, llm.ClassTransient, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := responsesServer(t, "", tc.handler)
			m, _ := llm.New(responsesConfig(ts.URL, "gpt-5.6-terra")).Model(context.Background(), "gpt-5.6-terra")
			ctx, info := llm.WithCallInfo(context.Background())

			// Act
			sr, err := m.Stream(ctx, []*schema.Message{schema.UserMessage("hi")})
			if err == nil {
				_, err = drain(t, sr)
			}

			// Assert
			if llm.Classify(err) != tc.want || info.RetryAfter() != tc.after {
				t.Fatalf("err %v class %q retryAfter %v", err, llm.Classify(err), info.RetryAfter())
			}
		})
	}
}

func TestResponsesToolsAreStrictOnlyWhenTheirSchemaAllows(t *testing.T) {
	// Function tools are sent with strict:true when every property is required and additionalProperties is false, otherwise strict:false; the last step's ToolChoiceForbidden is tool_choice none.
	// Arrange
	ts, got := responsesServer(t, "text.sse", nil)
	m, _ := llm.New(responsesConfig(ts.URL, "gpt-5.6-terra")).Model(context.Background(), "gpt-5.6-terra")
	var strict, loose map[string]any
	_ = json.Unmarshal([]byte(`{"type":"object","properties":{"expr":{"type":"string"}},"required":["expr"],"additionalProperties":false}`), &strict)
	_ = json.Unmarshal([]byte(`{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"integer"}},"required":["path"]}`), &loose)
	withTools, err := m.WithTools([]*schema.ToolInfo{
		{Name: "calc", Desc: "calc", ParamsOneOf: paramsOf(t, strict)},
		{Name: "read_file", Desc: "read", ParamsOneOf: paramsOf(t, loose)},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	_, err = withTools.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")}, model.WithToolChoice(schema.ToolChoiceForbidden))

	// Assert
	b := (*got)[0].body
	tools, _ := b["tools"].([]any)
	if err != nil || len(tools) != 2 || tools[0].(map[string]any)["strict"] != true || tools[1].(map[string]any)["strict"] != false ||
		b["parallel_tool_calls"] != true || b["tool_choice"] != "none" {
		t.Fatalf("err %v body %v", err, b)
	}
}

func TestOtherModelsStayOnChatCompletions(t *testing.T) {
	// Models outside LLM_RESPONSES_MODELS keep the Chat Completions path.
	// Arrange
	var paths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(ts.Close)
	cfg := openaiConfig(ts.URL, 10, "gpt-test")
	cfg.LLMResponsesModels = []string{"gpt-5.6-terra"}
	f := llm.New(cfg)

	// Act
	for _, name := range []string{"gpt-test"} {
		m, err := f.Model(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("ping")}); err != nil {
			t.Fatal(err)
		}
	}

	// Assert
	if len(paths) != 1 || paths[0] != "/v1/chat/completions" {
		t.Fatalf("paths %v", paths)
	}
}

func paramsOf(t *testing.T, s map[string]any) *schema.ParamsOneOf {
	t.Helper()
	b, _ := json.Marshal(s)
	var js jsonschema.Schema
	if err := json.Unmarshal(b, &js); err != nil {
		t.Fatal(err)
	}
	return schema.NewParamsOneOfByJSONSchema(&js)
}
