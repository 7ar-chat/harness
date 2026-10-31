package openai

import (
	"encoding/json"
	"fmt"
	"strings"
)

// APIError is a failed call: an HTTP error status or an `error` object in a stream. Classifiers read
// StatusCode and ErrorCode through interfaces, so they need not import this package.
type APIError struct {
	// Status is the HTTP status (0: the error arrived inside a stream).
	Status  int
	Code    string
	Type    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("openai: status code: %d, code: %s, message: %s", e.Status, e.Code, e.Message)
}

// StatusCode is the HTTP status of the failed call.
func (e *APIError) StatusCode() int { return e.Status }

// ErrorCode is the provider's error code ("context_length_exceeded", ...).
func (e *APIError) ErrorCode() string { return e.Code }

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
	if c := strings.TrimSpace(string(b.Code)); c != "" && c != "null" {
		return c
	}
	return ""
}

// httpError builds the error of a non-200 response from its JSON body.
func httpError(status int, body []byte) *APIError {
	var env struct {
		Error errorBody `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && (env.Error.Message != "" || len(env.Error.Code) > 0) {
		return &APIError{Status: status, Code: env.Error.code(), Type: env.Error.Type, Message: env.Error.Message}
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	return &APIError{Status: status, Message: msg}
}
