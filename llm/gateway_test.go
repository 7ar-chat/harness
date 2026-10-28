package llm_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/auth"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/llm"
)

type captured struct {
	path, query, useCase, userinfo, apiKey string
}

func fakeGateway(t *testing.T) (*httptest.Server, *captured) {
	t.Helper()
	c := &captured{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.path, c.query = r.URL.Path, r.URL.RawQuery
		c.useCase, c.userinfo, c.apiKey = r.Header.Get("X-Use-Case"), r.Header.Get("x-userinfo"), r.Header.Get("api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(ts.Close)
	return ts, c
}

func TestGatewayModelUsesAzurePathAndForwardsIdentity(t *testing.T) {
	// The gateway provider calls {gateway}/usecase/{uc}/providers/openai/models/{m}/openai/deployments/{m}/chat/completions with X-Use-Case and the user's x-userinfo.
	// Arrange
	ts, got := fakeGateway(t)
	f := llm.New(&config.Config{LLMProvider: "gateway", LLMGatewayURL: ts.URL + "/", LLMUseCase: "5", LLMAPIVersion: "2025-04-01-preview", LLMModels: []string{"gpt-test"}, LLMTimeout: 10})
	m, err := f.Model(context.Background(), "gpt-test")
	if err != nil {
		t.Fatal(err)
	}

	// Act
	out, err := m.Generate(auth.WithUserinfo(context.Background(), "eyJsb2dpbiI6ImEifQ=="), []*schema.Message{schema.UserMessage("ping")})

	// Assert
	if err != nil || out.Content != "pong" {
		t.Fatalf("err %v out %+v", err, out)
	}
	if got.path != "/usecase/5/providers/openai/models/gpt-test/openai/deployments/gpt-test/chat/completions" ||
		!strings.Contains(got.query, "api-version=2025-04-01-preview") || got.useCase != "5" || got.userinfo != "eyJsb2dpbiI6ImEifQ==" ||
		got.apiKey != llm.GatewayDummyKey {
		t.Fatalf("request %+v", got)
	}
}

func TestOpenAIProviderUsesPlainBaseURL(t *testing.T) {
	// The openai provider calls {LLM_BASE_URL}/chat/completions with the API key and no identity.
	// Arrange
	ts, got := fakeGateway(t)
	f := llm.New(&config.Config{LLMProvider: "openai", LLMBaseURL: ts.URL + "/v1", LLMAPIKey: "sk-test", LLMModels: []string{"gpt-test"}, LLMTimeout: 10})
	m, _ := f.Model(context.Background(), "gpt-test")

	// Act
	_, err := m.Generate(auth.WithUserinfo(context.Background(), "secret"), []*schema.Message{schema.UserMessage("ping")})

	// Assert
	if err != nil || got.path != "/v1/chat/completions" || got.userinfo != "" || got.useCase != "" {
		t.Fatalf("err %v request %+v", err, got)
	}
}

func TestModelOutsideAllowListIsRefused(t *testing.T) {
	// Models outside LLM_MODELS are refused.
	// Arrange
	f := llm.New(&config.Config{LLMProvider: "gateway", LLMModels: []string{"gpt-test"}})

	// Act
	_, err := f.Model(context.Background(), "gpt-evil")

	// Assert
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestReasoningEffortIsSentInTheRequest(t *testing.T) {
	// llm.WithReasoningEffort puts reasoning_effort in the chat completion request body.
	// Arrange
	var body string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(ts.Close)
	f := llm.New(&config.Config{LLMProvider: "openai", LLMBaseURL: ts.URL + "/v1", LLMAPIKey: "sk", LLMModels: []string{"gpt-test"}, LLMTimeout: 10})
	m, _ := f.Model(context.Background(), "gpt-test")

	// Act
	_, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("ping")}, llm.WithReasoningEffort("minimal")...)

	// Assert
	if err != nil || !strings.Contains(body, `"reasoning_effort":"minimal"`) {
		t.Fatalf("err %v body %s", err, body)
	}
}
