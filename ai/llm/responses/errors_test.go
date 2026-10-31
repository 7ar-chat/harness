package responses_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai/llm"
	"github.com/7ar-chat/harness/ai/llm/responses"
)

func TestHTTPErrorsKeepStatusCodeTypeAndMessage(t *testing.T) {
	// A failed call returns an APIError that exposes status and code to retry classification and renders all three facts; a numeric code is kept as its text and a null code is empty.
	// Arrange
	cases := []struct {
		name       string
		status     int
		body       string
		wantCode   string
		wantType   string
		wantMsg    string
		wantStatus int
	}{
		{"string code", 429, `{"error":{"code":"rate_limit_exceeded","type":"requests","message":"slow down"}}`, "rate_limit_exceeded", "requests", "slow down", 429},
		{"numeric code", 429, `{"error":{"code":429,"message":"token rate limit"}}`, "429", "", "token rate limit", 429},
		{"null code", 400, `{"error":{"code":null,"type":"invalid_request_error","message":"bad"}}`, "", "invalid_request_error", "bad", 400},
		{"plain text body", 502, "  upstream down\n", "", "", "upstream down", 502},
		{"json without error", 500, `{"detail":"x"}`, "", "", `{"detail":"x"}`, 500},
		{"error without message", 503, `{"error":{"code":"overloaded"}}`, "overloaded", "", `{"error":{"code":"overloaded"}}`, 503},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, _ := serve(t, failure(tc.status, tc.body))

			// Act
			_, err := newModel(t, url).Generate(context.Background(), hi())

			// Assert
			var apiErr *responses.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err %v, want an *APIError", err)
			}
			if apiErr.StatusCode() != tc.wantStatus || apiErr.ErrorCode() != tc.wantCode || apiErr.Type != tc.wantType || apiErr.Message != tc.wantMsg {
				t.Fatalf("error %#v", apiErr)
			}
			for _, part := range []string{"status code: " + strconv.Itoa(tc.wantStatus), "code: " + tc.wantCode, "message: " + tc.wantMsg} {
				if !strings.Contains(apiErr.Error(), part) {
					t.Errorf("Error() = %q, missing %q", apiErr.Error(), part)
				}
			}
		})
	}
}

func TestLongNonJSONErrorBodyIsTruncated(t *testing.T) {
	// A huge HTML error page does not end up whole in the error message.
	// Arrange
	url, _ := serve(t, failure(502, strings.Repeat("x", 5000)))

	// Act
	_, err := newModel(t, url).Generate(context.Background(), hi())

	// Assert
	var apiErr *responses.APIError
	if !errors.As(err, &apiErr) || len(apiErr.Message) != 500 {
		t.Fatalf("err %v, want a message cut to 500 bytes", err)
	}
}

func TestErrorsAreClassifiedForTheAgentRetryPolicy(t *testing.T) {
	// HTTP failures and stream error events get the retry class their status/code implies: throttling is rate-limited, server trouble is transient, an exceeded context is an overflow, anything else the provider rejects is fatal.
	// Arrange
	streamErr := func(fields map[string]any) func(w http.ResponseWriter) { return stream(event("error", fields)) }
	cases := []struct {
		name  string
		reply func(w http.ResponseWriter)
		want  llm.ErrorClass
	}{
		{"http 429", failure(429, `{"error":{"message":"slow"}}`), llm.ClassRateLimited},
		{"http 500", failure(500, `{"error":{"message":"boom"}}`), llm.ClassTransient},
		{"http 400 overflow", failure(400, `{"error":{"code":"context_length_exceeded","message":"too big"}}`), llm.ClassOverflow},
		{"http 401", failure(401, `{"error":{"message":"bad key"}}`), llm.ClassFatal},
		{"event rate limit", streamErr(map[string]any{"code": "rate_limit_exceeded"}), llm.ClassRateLimited},
		{"event too many requests", streamErr(map[string]any{"error": map[string]any{"type": "too_many_requests"}}), llm.ClassRateLimited},
		{"event overflow", streamErr(map[string]any{"code": "context_length_exceeded", "message": "full"}), llm.ClassOverflow},
		{"event server error", streamErr(map[string]any{"code": "server_error"}), llm.ClassTransient},
		{"event internal", streamErr(map[string]any{"code": "internal_error"}), llm.ClassTransient},
		{"event overloaded", streamErr(map[string]any{"code": "overloaded"}), llm.ClassTransient},
		{"event unavailable", streamErr(map[string]any{"error": map[string]any{"type": "service_unavailable"}}), llm.ClassTransient},
		{"event timeout", streamErr(map[string]any{"code": "timeout"}), llm.ClassTransient},
		{"event invalid", streamErr(map[string]any{"code": "invalid_prompt"}), llm.ClassFatal},
		{"event not found", streamErr(map[string]any{"code": "model_not_found"}), llm.ClassFatal},
		{"event unknown", streamErr(map[string]any{"code": "mystery"}), llm.ClassFatal},
		{"cut stream", stream(""), llm.ClassTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, _ := serve(t, tc.reply)

			// Act
			_, err := streamAll(t, newModel(t, url), hi())

			// Assert
			if got := llm.Classify(err); got != tc.want {
				t.Fatalf("err %v classified %q, want %q", err, got, tc.want)
			}
		})
	}
}
