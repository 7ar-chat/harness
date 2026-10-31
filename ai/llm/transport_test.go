package llm_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/llm"
)

func openaiConfig(url string, timeout int, models ...string) llm.Options {
	return llm.Options{Provider: "openai", BaseURL: url + "/v1", APIKey: "test-key", Models: models, TimeoutSeconds: timeout}
}

func TestModelCallTimesOutWhenTheGatewayNeverAnswers(t *testing.T) {
	// Options.TimeoutSeconds bounds the wait for the endpoint's response headers.
	// Arrange
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); ts.Close() })
	m, err := llm.New(openaiConfig(ts.URL, 1, "gpt-test")).Model(context.Background(), "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	// Act
	start := time.Now()
	_, err = m.Stream(ctx, []*ai.Message{ai.UserMessage("ping")})
	elapsed := time.Since(start)

	// Assert
	if err == nil || elapsed > 2500*time.Millisecond || llm.Classify(err) != llm.ClassTimeout {
		t.Fatalf("err %v (class %q) after %v; want a timeout after ~1s", err, llm.Classify(err), elapsed)
	}
}

func TestRateLimitedCallRecordsRetryAfter(t *testing.T) {
	// A 429 with Retry-After hands the provider's delay to the caller's CallInfo.
	// Arrange
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"429","message":"Requests to the ChatCompletions_Create Operation have exceeded token rate limit"}}`))
	}))
	t.Cleanup(ts.Close)
	m, _ := llm.New(openaiConfig(ts.URL, 10, "gpt-test")).Model(context.Background(), "gpt-test")
	ctx, info := llm.WithCallInfo(context.Background())

	// Act
	_, err := m.Stream(ctx, []*ai.Message{ai.UserMessage("ping")})

	// Assert
	if llm.Classify(err) != llm.ClassRateLimited || info.RetryAfter() != 7*time.Second {
		t.Fatalf("err %v class %q retryAfter %v", err, llm.Classify(err), info.RetryAfter())
	}
}

func TestStreamedRefusalIsRecorded(t *testing.T) {
	// delta.refusal chunks reach the caller's CallInfo.
	// Arrange
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, d := range []string{
			`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"","refusal":null}}]}`,
			`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"refusal":"I'm sorry, "}}]}`,
			`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"refusal":"I can't help with that."},"finish_reason":"stop"}]}`,
		} {
			_, _ = w.Write([]byte("data: " + d + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(ts.Close)
	m, _ := llm.New(openaiConfig(ts.URL, 10, "gpt-test")).Model(context.Background(), "gpt-test")
	ctx, info := llm.WithCallInfo(context.Background())

	// Act
	sr, err := m.Stream(ctx, []*ai.Message{ai.UserMessage("ping")})
	if err == nil {
		for {
			if _, rerr := sr.Recv(); rerr != nil {
				break
			}
		}
	}

	// Assert
	if err != nil || info.Refusal() != "I'm sorry, I can't help with that." {
		t.Fatalf("err %v refusal %q", err, info.Refusal())
	}
}

func TestModelIDIsSentVerbatim(t *testing.T) {
	// A model id with a slash (OpenRouter style) reaches {BaseURL}/chat/completions unchanged, with the bearer key.
	// Arrange
	var path, auth, body string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(ts.Close)
	m, err := llm.New(openaiConfig(ts.URL, 10, "deepseek/DeepSeek-V4-Flash")).Model(context.Background(), "deepseek/DeepSeek-V4-Flash")
	if err != nil {
		t.Fatal(err)
	}

	// Act
	out, err := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("ping")})

	// Assert
	if err != nil || out.Content != "pong" || path != "/v1/chat/completions" || auth != "Bearer test-key" ||
		!strings.Contains(body, `"model":"deepseek/DeepSeek-V4-Flash"`) {
		t.Fatalf("err %v path %s auth %s body %s", err, path, auth, body)
	}
}

func TestPromptCacheKeyReachesTheRequestBody(t *testing.T) {
	// llm.WithPromptCacheKey adds prompt_cache_key to the Chat Completions body.
	// Arrange
	var body string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(ts.Close)
	m, _ := llm.New(openaiConfig(ts.URL, 10, "gpt-test")).Model(context.Background(), "gpt-test")

	// Act
	_, err := m.Generate(context.Background(), []*ai.Message{ai.UserMessage("ping")}, ai.WithPromptCacheKey("thread-1"))

	// Assert
	if err != nil || !strings.Contains(body, `"prompt_cache_key":"thread-1"`) {
		t.Fatalf("err %v body %s", err, body)
	}
}
