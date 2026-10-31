// Package anthropic is an ai.Model over the Anthropic Messages
// API (POST /v1/messages, streamed). It sets prompt-cache breakpoints on the
// tools, the system prompt and the last message, so a long agent history is
// read from the cache on every step, and reports cache reads and writes in the
// usage of the answer.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/GhaziBenDahmane/harness/ai"
)

const (
	// DefaultBaseURL is the Anthropic API.
	DefaultBaseURL = "https://api.anthropic.com"
	// DefaultMaxTokens is max_tokens when a call does not set one.
	DefaultMaxTokens = 16384
	apiVersion       = "2023-06-01"
)

// Config configures a model.
type Config struct {
	BaseURL string // default DefaultBaseURL
	APIKey  string
	// Model is the model id sent in the body.
	Model      string
	HTTPClient *http.Client
	// Header is added to every request (e.g. a proxy's headers).
	Header http.Header
	// CacheTTL is the prompt-cache lifetime of the breakpoints: "" or "5m", or "1h".
	CacheTTL string
	// NoCache turns the cache breakpoints off.
	NoCache bool
}

// Model is an Anthropic-backed ai.Model.
type Model struct {
	cfg   *Config
	tools []tool
}

var _ ai.Model = (*Model)(nil)

// New returns a model.
func New(cfg Config) (*Model, error) {
	if cfg.Model == "" {
		return nil, errors.New("anthropic: Model is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	return &Model{cfg: &cfg}, nil
}

// WithTools implements ai.Model.
func (m *Model) WithTools(infos []*ai.ToolInfo) (ai.Model, error) {
	tools, err := toTools(infos)
	if err != nil {
		return nil, err
	}
	return &Model{cfg: m.cfg, tools: tools}, nil
}

// Generate implements ai.ChatModel (a streamed call, concatenated).
func (m *Model) Generate(ctx context.Context, msgs []*ai.Message, opts ...ai.Option) (*ai.Message, error) {
	sr, err := m.Stream(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	defer sr.Close()
	var chunks []*ai.Message
	for {
		c, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, c)
	}
	if len(chunks) == 0 {
		return &ai.Message{Role: ai.RoleAssistant}, nil
	}
	return ai.ConcatMessages(chunks)
}

// Stream implements ai.ChatModel.
func (m *Model) Stream(ctx context.Context, msgs []*ai.Message, opts ...ai.Option) (*ai.StreamReader[*ai.Message], error) {
	body, err := m.request(msgs, opts)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(m.cfg.BaseURL, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range m.cfg.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("anthropic-version", apiVersion)
	if m.cfg.APIKey != "" {
		req.Header.Set("x-api-key", m.cfg.APIKey)
	}
	resp, err := m.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, httpError(resp.StatusCode, b)
	}
	sr, sw := ai.Pipe[*ai.Message](16)
	go func() {
		defer sw.Close()
		defer resp.Body.Close()
		(&streamState{ctx: ctx, sw: sw}).run(resp.Body)
	}()
	return sr, nil
}

// APIError is a failed call. StatusCode lets llm.Classify sort it (429 rate
// limit, 5xx and 529 transient, 400 "prompt is too long" overflow, other 4xx fatal).
type APIError struct {
	Status  int
	Type    string
	Message string
}

// Error implements error.
func (e *APIError) Error() string {
	return fmt.Sprintf("anthropic: status code: %d, type: %s, message: %s", e.Status, e.Type, e.Message)
}

// StatusCode implements the interface llm.Classify reads.
func (e *APIError) StatusCode() int { return e.Status }

func httpError(status int, body []byte) *APIError {
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	e := &APIError{Status: status}
	if json.Unmarshal(body, &env) == nil {
		e.Type, e.Message = env.Error.Type, env.Error.Message
	}
	if e.Message == "" {
		e.Message = strings.TrimSpace(string(body))
		if len(e.Message) > 500 {
			e.Message = e.Message[:500]
		}
	}
	return e
}

// statusOfType maps the type of an in-stream error event to the HTTP status it stands for.
func statusOfType(t string) int {
	switch t {
	case "rate_limit_error":
		return http.StatusTooManyRequests
	case "overloaded_error":
		return 529
	case "invalid_request_error":
		return http.StatusBadRequest
	case "authentication_error":
		return http.StatusUnauthorized
	case "permission_error":
		return http.StatusForbidden
	case "not_found_error":
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}
