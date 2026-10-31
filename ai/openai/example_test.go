package openai_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/openai"
)

// New returns a model for any server that speaks the Chat Completions protocol: OpenAI, Azure, OpenRouter, vLLM,
// Ollama, a gateway. BaseURL is the API root, the path /chat/completions is added. The server here is a stand-in.
func ExampleNew() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"Bonjour !"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":9,"completion_tokens":3,"total_tokens":12}}`)
	}))
	defer srv.Close()

	model, err := openai.New(openai.Config{BaseURL: srv.URL + "/v1", APIKey: "sk-test", Model: "gpt-4o-mini"})
	if err != nil {
		panic(err)
	}
	reply, err := model.Generate(context.Background(), []*ai.Message{
		ai.SystemMessage("Answer in French."),
		ai.UserMessage("Say hello."),
	}, ai.WithTemperature(0))

	fmt.Println(reply.Content, "|", reply.ResponseMeta.Usage.TotalTokens, "tokens |", err)
	// Output:
	// Bonjour ! | 12 tokens | <nil>
}

// A failed call is an *APIError carrying the HTTP status and the provider's error code; ai/llm.Classify reads both to
// decide whether to retry.
func ExampleAPIError() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`)
	}))
	defer srv.Close()
	model, _ := openai.New(openai.Config{BaseURL: srv.URL, Model: "m"})

	_, err := model.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")})

	apiErr := err.(*openai.APIError)
	fmt.Println(apiErr.StatusCode(), apiErr.ErrorCode(), apiErr.Message)
	// Output:
	// 429 rate_limit_exceeded slow down
}
