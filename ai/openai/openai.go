// Package openai is an ai.Model over the OpenAI Chat Completions API (POST {BaseURL}/chat/completions),
// as served by OpenAI and the many gateways that copy it. Stream reads server-sent events; Generate
// makes one non-streamed call.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/GhaziBenDahmane/harness/ai"
)

// Config configures a Model.
type Config struct {
	// BaseURL is the API root ("https://api.openai.com/v1" when empty); requests go to BaseURL/chat/completions.
	BaseURL string
	APIKey  string
	// Model is the model id, sent verbatim.
	Model string
	// HTTPClient sends the requests (its transport bounds connecting and the response headers).
	HTTPClient *http.Client
}

// Model is a Chat Completions backed ai.Model.
type Model struct {
	cfg   *Config
	tools []tool
}

var _ ai.Model = (*Model)(nil)

// New returns a Model.
func New(cfg Config) (*Model, error) {
	if cfg.Model == "" {
		return nil, errors.New("openai: Model is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.openai.com/v1"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	return &Model{cfg: &cfg}, nil
}

// WithTools implements ai.Model.
func (m *Model) WithTools(infos []*ai.ToolInfo) (ai.Model, error) {
	return &Model{cfg: m.cfg, tools: toTools(infos)}, nil
}

// Generate implements ai.ChatModel with one non-streamed call.
func (m *Model) Generate(ctx context.Context, msgs []*ai.Message, opts ...ai.Option) (*ai.Message, error) {
	body, err := m.request(msgs, opts, false)
	if err != nil {
		return nil, err
	}
	resp, err := m.post(ctx, body, "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parseCompletion(raw)
}

// Stream implements ai.ChatModel.
func (m *Model) Stream(ctx context.Context, msgs []*ai.Message, opts ...ai.Option) (*ai.StreamReader[*ai.Message], error) {
	body, err := m.request(msgs, opts, true)
	if err != nil {
		return nil, err
	}
	resp, err := m.post(ctx, body, "text/event-stream")
	if err != nil {
		return nil, err
	}
	sr, sw := ai.Pipe[*ai.Message](16)
	go func() {
		defer sw.Close()
		defer resp.Body.Close()
		readStream(ctx, resp.Body, sw)
	}()
	return sr, nil
}

func (m *Model) post(ctx context.Context, body []byte, accept string) (*http.Response, error) {
	url := strings.TrimRight(m.cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if m.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	resp, err := m.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, httpError(resp.StatusCode, b)
	}
	return resp, nil
}

type tool struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

func toTools(infos []*ai.ToolInfo) []tool {
	out := make([]tool, 0, len(infos))
	for _, info := range infos {
		if info == nil {
			continue
		}
		params := info.Parameters
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, tool{Type: "function", Function: toolFunction{Name: info.Name, Description: info.Desc, Parameters: params}})
	}
	return out
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type requestBody struct {
	Model           string         `json:"model"`
	Messages        []wireMessage  `json:"messages"`
	Stream          bool           `json:"stream,omitempty"`
	StreamOptions   *streamOptions `json:"stream_options,omitempty"`
	Tools           []tool         `json:"tools,omitempty"`
	ToolChoice      any            `json:"tool_choice,omitempty"`
	Temperature     *float32       `json:"temperature,omitempty"`
	TopP            *float32       `json:"top_p,omitempty"`
	MaxTokens       *int           `json:"max_tokens,omitempty"`
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
	PromptCacheKey  string         `json:"prompt_cache_key,omitempty"`
}

func (m *Model) request(msgs []*ai.Message, opts []ai.Option, stream bool) ([]byte, error) {
	o := ai.Apply(opts...)
	body := requestBody{
		Model: m.cfg.Model, Messages: toWire(msgs), Stream: stream, Tools: m.tools,
		Temperature: o.Temperature, TopP: o.TopP, MaxTokens: o.MaxTokens,
		ReasoningEffort: o.ReasoningEffort, PromptCacheKey: o.PromptCacheKey,
	}
	if stream {
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	if len(m.tools) > 0 {
		body.ToolChoice = toolChoice(o)
	}
	return json.Marshal(body)
}

func toolChoice(o ai.CallOptions) any {
	if o.ToolChoice == nil {
		return nil
	}
	switch *o.ToolChoice {
	case ai.ToolChoiceForbidden:
		return "none"
	case ai.ToolChoiceForced:
		if len(o.AllowedToolNames) == 1 {
			return map[string]any{"type": "function", "function": map[string]any{"name": o.AllowedToolNames[0]}}
		}
		return "required"
	}
	return "auto"
}
