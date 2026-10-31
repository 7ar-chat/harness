package responses

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// APIError is a failed Responses call: an HTTP error status, an `error` event
// or a `response.failed` event. StatusCode lets llm.Classify sort it like the
// Chat Completions errors (429 rate limit, 5xx transient, 400 with
// context_length_exceeded overflow, other 4xx fatal).
type APIError struct {
	// Status is the HTTP status, or the status the error code of a stream event maps to (0: unknown).
	Status  int
	Code    string
	Type    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("responses: status code: %d, code: %s, message: %s", e.Status, e.Code, e.Message)
}

// StatusCode implements the interface llm.Classify reads.
func (e *APIError) StatusCode() int { return e.Status }

// ErrorCode is the provider's error code, read by llm.Classify.
func (e *APIError) ErrorCode() string { return e.Code }

// rejectsReasoning reports an error about the reasoning items of the request
// (invalid_encrypted_content after a model or key change, an item the provider
// no longer accepts).
func (e *APIError) rejectsReasoning() bool {
	if e.Status != http.StatusBadRequest && e.Status != http.StatusNotFound {
		return false
	}
	msg := strings.ToLower(e.Code + " " + e.Message)
	return strings.Contains(msg, "encrypted") || (strings.Contains(msg, "reasoning") && strings.Contains(msg, "item")) ||
		strings.Contains(msg, "item with id 'rs_")
}

type errorBody struct {
	Code    json.RawMessage `json:"code"`
	Type    string          `json:"type"`
	Message string          `json:"message"`
}

func (b errorBody) code() string {
	var s string
	if json.Unmarshal(b.Code, &s) == nil {
		return s
	}
	if len(b.Code) > 0 && string(b.Code) != "null" {
		return string(b.Code)
	}
	return ""
}

// httpError builds the error of a non-200 response from its JSON body.
func httpError(status int, body []byte) *APIError {
	var env struct {
		Error *errorBody `json:"error"`
	}
	e := &APIError{Status: status}
	if json.Unmarshal(body, &env) == nil && env.Error != nil {
		e.Code, e.Type, e.Message = env.Error.code(), env.Error.Type, env.Error.Message
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(body))
		if len(e.Message) > 500 {
			e.Message = e.Message[:500]
		}
	}
	return e
}

// streamError builds the error of an `error` / `response.failed` event: its
// code decides the status the classification sees.
func streamError(code, typ, message string) *APIError {
	return &APIError{Status: statusOfCode(code, typ), Code: code, Type: typ, Message: message}
}

func statusOfCode(code, typ string) int {
	c := strings.ToLower(code + " " + typ)
	switch {
	case strings.Contains(c, "rate_limit") || strings.Contains(c, "too_many_requests"):
		return http.StatusTooManyRequests
	case strings.Contains(c, "context_length_exceeded"):
		return http.StatusBadRequest
	case strings.Contains(c, "server_error") || strings.Contains(c, "internal_error") || strings.Contains(c, "overloaded") ||
		strings.Contains(c, "unavailable") || strings.Contains(c, "timeout"):
		return http.StatusInternalServerError
	case strings.Contains(c, "invalid") || strings.Contains(c, "content_filter") || strings.Contains(c, "not_found"):
		return http.StatusBadRequest
	}
	return 0
}
