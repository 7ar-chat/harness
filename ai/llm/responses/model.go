// Package responses is a ai.Model over the OpenAI Responses
// API (POST …/responses, streamed), as served by the OpenAI-compatible endpoint
// (docs/spike-responses-api.md). Calls are stateless (store:false) and ask for
// the encrypted reasoning (include reasoning.encrypted_content): the reasoning
// items of an answer travel on its ai.Message (ExtraReasoningItems) and
// are sent back, in order, before the answer's own items, so the model keeps
// its reasoning across tool steps and runs.
package responses

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/GhaziBenDahmane/harness/ai"
)

// Config configures a Responses model.
type Config struct {
	// URL is the full POST URL, e.g. https://api.openai.com/v1/responses.
	URL string
	// Model is the model sent in the body (verbatim).
	Model string
	// Name is the Work model id: reasoning items are tagged with it and only sent back to it.
	Name string
	// HTTPClient sends the requests (the llm provider's client: timeouts, CallInfo).
	HTTPClient *http.Client
	// Header is added to every request (api-key or Authorization).
	Header http.Header
	// OnRefusal receives streamed refusal text (the agent treats a refusal like an empty answer).
	OnRefusal func(ctx context.Context, text string)
	// ReasoningSummary is reasoning.summary ("": none sent).
	ReasoningSummary string
}

// Model is a Responses-backed ai.Model.
type Model struct {
	cfg      *Config
	tools    []functionTool
	rejected *rejectedSet
}

var _ ai.Model = (*Model)(nil)

// New returns a Responses model.
func New(cfg Config) (*Model, error) {
	if cfg.URL == "" || cfg.Model == "" {
		return nil, errors.New("responses: URL and Model are required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.Name == "" {
		cfg.Name = cfg.Model
	}
	return &Model{cfg: &cfg, rejected: &rejectedSet{ids: map[string]struct{}{}}}, nil
}

// WithTools implements ai.Model.
func (m *Model) WithTools(infos []*ai.ToolInfo) (ai.Model, error) {
	tools, err := toFunctionTools(infos)
	if err != nil {
		return nil, err
	}
	return &Model{cfg: m.cfg, tools: tools, rejected: m.rejected}, nil
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

// Stream implements ai.ChatModel. A request whose reasoning items the
// provider rejects (invalid_encrypted_content, e.g. after a key change) is
// re-sent once without them; the rejected items are not sent again.
func (m *Model) Stream(ctx context.Context, msgs []*ai.Message, opts ...ai.Option) (*ai.StreamReader[*ai.Message], error) {
	body, sent, err := m.request(msgs, opts, true)
	if err != nil {
		return nil, err
	}
	resp, err := m.post(ctx, body)
	var apiErr *APIError
	if err != nil && len(sent) > 0 && errors.As(err, &apiErr) && apiErr.rejectsReasoning() {
		slog.Warn("llm: the provider rejected the reasoning items; retrying once without them",
			"model", m.cfg.Name, "items", len(sent), "error", apiErr.Message)
		m.rejected.add(sent)
		if body, _, err = m.request(msgs, opts, false); err != nil {
			return nil, err
		}
		resp, err = m.post(ctx, body)
	}
	if err != nil {
		return nil, err
	}
	sr, sw := ai.Pipe[*ai.Message](16)
	go func() {
		defer sw.Close()
		defer resp.Body.Close()
		(&streamState{cfg: m.cfg, ctx: ctx, sw: sw}).run(resp.Body)
	}()
	return sr, nil
}

func (m *Model) post(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.URL, bytes.NewReader(body))
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

type reasoningParam struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type requestBody struct {
	Model             string          `json:"model"`
	Input             []any           `json:"input"`
	Stream            bool            `json:"stream"`
	Store             bool            `json:"store"`
	Include           []string        `json:"include"`
	Reasoning         *reasoningParam `json:"reasoning,omitempty"`
	Tools             []functionTool  `json:"tools,omitempty"`
	ToolChoice        any             `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	PromptCacheKey    string          `json:"prompt_cache_key,omitempty"`
	MaxOutputTokens   *int            `json:"max_output_tokens,omitempty"`
	Temperature       *float32        `json:"temperature,omitempty"`
	TopP              *float32        `json:"top_p,omitempty"`
}

// request builds the body; sent lists the ids of the reasoning items it carries.
func (m *Model) request(msgs []*ai.Message, opts []ai.Option, withReasoning bool) ([]byte, []string, error) {
	common := ai.Apply(opts...)
	tools := m.tools
	input, sent := m.input(msgs, withReasoning)
	body := requestBody{
		Model: m.cfg.Model, Input: input, Stream: true, Store: false,
		Include:         []string{"reasoning.encrypted_content"},
		PromptCacheKey:  common.PromptCacheKey,
		MaxOutputTokens: common.MaxTokens, Temperature: common.Temperature, TopP: common.TopP,
	}
	if common.ReasoningEffort != "" || m.cfg.ReasoningSummary != "" {
		body.Reasoning = &reasoningParam{Effort: common.ReasoningEffort, Summary: m.cfg.ReasoningSummary}
	}
	if len(tools) > 0 {
		parallel := true
		body.Tools, body.ParallelToolCalls = tools, &parallel
		body.ToolChoice = toolChoice(common)
	}
	b, err := json.Marshal(body)
	return b, sent, err
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
			return map[string]any{"type": "function", "name": o.AllowedToolNames[0]}
		}
		return "required"
	}
	return "auto"
}

type messageItem struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type reasoningInput struct {
	Type             string        `json:"type"`
	ID               string        `json:"id"`
	Summary          []SummaryPart `json:"summary"`
	EncryptedContent string        `json:"encrypted_content"`
}

type functionCallItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type functionCallOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// input converts the history into Responses input items: an assistant message
// becomes its reasoning items (those of this model, not rejected), its text,
// then its function calls; a tool message becomes a function_call_output.
func (m *Model) input(msgs []*ai.Message, withReasoning bool) ([]any, []string) {
	out := make([]any, 0, len(msgs)+4)
	var sent []string
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		switch msg.Role {
		case ai.RoleSystem:
			out = append(out, messageItem{Type: "message", Role: "system", Content: msg.Content})
		case ai.RoleUser:
			out = append(out, messageItem{Type: "message", Role: "user", Content: userContent(msg)})
		case ai.RoleAssistant:
			// A reasoning item must be followed by the output it produced.
			if withReasoning && (msg.Content != "" || len(msg.ToolCalls) > 0) {
				for _, it := range ItemsOf(msg) {
					if it.ID == "" || it.EncryptedContent == "" || it.Model != m.cfg.Name || m.rejected.has(it.ID) {
						continue
					}
					summary := it.Summary
					if summary == nil {
						summary = []SummaryPart{}
					}
					out = append(out, reasoningInput{Type: "reasoning", ID: it.ID, Summary: summary, EncryptedContent: it.EncryptedContent})
					sent = append(sent, it.ID)
				}
			}
			if msg.Content != "" {
				out = append(out, messageItem{Type: "message", Role: "assistant", Content: msg.Content})
			}
			for _, tc := range msg.ToolCalls {
				args := tc.Function.Arguments
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				out = append(out, functionCallItem{Type: "function_call", CallID: tc.ID, Name: tc.Function.Name, Arguments: args})
			}
		case ai.RoleTool:
			out = append(out, functionCallOutput{Type: "function_call_output", CallID: msg.ToolCallID, Output: msg.Content})
		}
	}
	return out, sent
}

func userContent(msg *ai.Message) any {
	if len(msg.Parts) > 0 {
		parts := make([]contentPart, 0, len(msg.Parts))
		for _, p := range msg.Parts {
			switch p.Type {
			case ai.PartText:
				parts = append(parts, contentPart{Type: "input_text", Text: p.Text})
			case ai.PartImage:
				if p.Image == nil {
					continue
				}
				if u := imageURL(*p.Image); u != "" {
					parts = append(parts, contentPart{Type: "input_image", ImageURL: u, Detail: detail(string(p.Image.Detail))})
				}
			}
		}
		return parts
	}
	return msg.Content
}

func imageURL(c ai.Image) string {
	if c.URL != "" {
		return c.URL
	}
	if c.Base64Data != "" {
		mime := c.MIMEType
		if mime == "" {
			mime = "image/png"
		}
		return "data:" + mime + ";base64," + c.Base64Data
	}
	return ""
}

func detail(d string) string {
	if d == "" {
		return "auto"
	}
	return d
}

// rejectedSet remembers reasoning items the provider refused (bounded).
type rejectedSet struct {
	mu  sync.Mutex
	ids map[string]struct{}
}

const maxRejected = 10000

func (s *rejectedSet) add(ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ids)+len(ids) > maxRejected {
		s.ids = map[string]struct{}{}
	}
	for _, id := range ids {
		s.ids[id] = struct{}{}
	}
}

func (s *rejectedSet) has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.ids[id]
	return ok
}
