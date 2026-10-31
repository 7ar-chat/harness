package anthropic

import (
	"encoding/json"
	"strings"

	"github.com/GhaziBenDahmane/harness/ai"
)

type cacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

// block is a content block of a request; Cache, when set, is a cache breakpoint after it.
type block struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	// image
	Source *imageSource  `json:"source,omitempty"`
	Cache  *cacheControl `json:"cache_control,omitempty"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type message struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
	Cache       *cacheControl  `json:"cache_control,omitempty"`
}

type requestBody struct {
	Model       string         `json:"model"`
	MaxTokens   int            `json:"max_tokens"`
	Stream      bool           `json:"stream"`
	System      []block        `json:"system,omitempty"`
	Messages    []message      `json:"messages"`
	Tools       []tool         `json:"tools,omitempty"`
	ToolChoice  map[string]any `json:"tool_choice,omitempty"`
	Temperature *float32       `json:"temperature,omitempty"`
	TopP        *float32       `json:"top_p,omitempty"`
}

func toTools(infos []*ai.ToolInfo) ([]tool, error) {
	out := make([]tool, 0, len(infos))
	for _, info := range infos {
		if info == nil {
			continue
		}
		params := cloneSchema(info.Parameters)
		delete(params, "$schema")
		if _, ok := params["properties"]; !ok && params["type"] == "object" {
			params["properties"] = map[string]any{}
		}
		out = append(out, tool{Name: info.Name, Description: info.Desc, InputSchema: params})
	}
	return out, nil
}

func (m *Model) breakpoint() *cacheControl {
	if m.cfg.NoCache {
		return nil
	}
	return &cacheControl{Type: "ephemeral", TTL: map[bool]string{true: "1h"}[m.cfg.CacheTTL == "1h"]}
}

func (m *Model) request(msgs []*ai.Message, opts []ai.Option) ([]byte, error) {
	common := ai.Apply(opts...)
	tools := m.tools
	body := requestBody{Model: m.cfg.Model, MaxTokens: DefaultMaxTokens, Stream: true, Temperature: common.Temperature, TopP: common.TopP}
	if common.MaxTokens != nil && *common.MaxTokens > 0 {
		body.MaxTokens = *common.MaxTokens
	}
	body.System, body.Messages = m.convert(msgs)
	bp := m.breakpoint()
	// Breakpoints: after the tools, after the system prompt, after the last message.
	if bp != nil {
		if len(tools) > 0 {
			c := *bp
			tools = append(append([]tool(nil), tools[:len(tools)-1]...), withCache(tools[len(tools)-1], &c))
		}
		if n := len(body.System); n > 0 {
			c := *bp
			body.System[n-1].Cache = &c
		}
		if n := len(body.Messages); n > 0 {
			last := &body.Messages[n-1]
			c := *bp
			last.Content[len(last.Content)-1].Cache = &c
		}
	}
	if len(tools) > 0 {
		body.Tools = tools
		body.ToolChoice = toolChoice(common)
	}
	return json.Marshal(body)
}

func withCache(t tool, c *cacheControl) tool { t.Cache = c; return t }

func toolChoice(o ai.CallOptions) map[string]any {
	if o.ToolChoice == nil {
		return nil
	}
	switch *o.ToolChoice {
	case ai.ToolChoiceForbidden:
		return map[string]any{"type": "none"}
	case ai.ToolChoiceForced:
		if len(o.AllowedToolNames) == 1 {
			return map[string]any{"type": "tool", "name": o.AllowedToolNames[0]}
		}
		return map[string]any{"type": "any"}
	}
	return map[string]any{"type": "auto"}
}

// convert turns the history into a system prompt and alternating user/assistant
// messages: tool results become tool_result blocks of a user message, first in it.
func (m *Model) convert(msgs []*ai.Message) ([]block, []message) {
	var system []block
	var out []message
	push := func(role string, bs ...block) {
		if len(bs) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, bs...)
			return
		}
		out = append(out, message{Role: role, Content: bs})
	}
	for _, msg := range msgs {
		if msg == nil {
			continue
		}
		switch msg.Role {
		case ai.RoleSystem:
			if strings.TrimSpace(msg.Content) != "" {
				system = append(system, block{Type: "text", Text: msg.Content})
			}
		case ai.RoleUser:
			bs := userBlocks(msg)
			if len(bs) == 0 {
				bs = []block{{Type: "text", Text: "(empty message)"}}
			}
			push("user", bs...)
		case ai.RoleAssistant:
			var bs []block
			if strings.TrimSpace(msg.Content) != "" {
				bs = append(bs, block{Type: "text", Text: msg.Content})
			}
			for _, tc := range msg.ToolCalls {
				input := map[string]any{}
				if a := strings.TrimSpace(tc.Function.Arguments); a != "" {
					if json.Unmarshal([]byte(a), &input) != nil || input == nil {
						input = map[string]any{}
					}
				}
				bs = append(bs, block{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input})
			}
			push("assistant", bs...)
		case ai.RoleTool:
			b := block{Type: "tool_result", ToolUseID: msg.ToolCallID, Content: msg.Content}
			// tool_result blocks come first in their user message.
			if n := len(out); n > 0 && out[n-1].Role == "user" {
				i := 0
				for i < len(out[n-1].Content) && out[n-1].Content[i].Type == "tool_result" {
					i++
				}
				c := out[n-1].Content
				out[n-1].Content = append(c[:i:i], append([]block{b}, c[i:]...)...)
			} else {
				push("user", b)
			}
		}
	}
	return system, out
}

func userBlocks(msg *ai.Message) []block {
	var bs []block
	if len(msg.Parts) > 0 {
		for _, p := range msg.Parts {
			switch p.Type {
			case ai.PartText:
				if p.Text != "" {
					bs = append(bs, block{Type: "text", Text: p.Text})
				}
			case ai.PartImage:
				if p.Image != nil {
					if b, ok := imageBlock(*p.Image); ok {
						bs = append(bs, b)
					}
				}
			}
		}
		return bs
	}
	if msg.Content != "" {
		bs = append(bs, block{Type: "text", Text: msg.Content})
	}
	return bs
}

func imageBlock(c ai.Image) (block, bool) {
	if c.Base64Data != "" {
		mime := c.MIMEType
		if mime == "" {
			mime = "image/png"
		}
		return block{Type: "image", Source: &imageSource{Type: "base64", MediaType: mime, Data: c.Base64Data}}, true
	}
	if c.URL != "" {
		return imageFromURL(c.URL), true
	}
	return block{}, false
}

func imageFromURL(u string) block {
	if rest, ok := strings.CutPrefix(u, "data:"); ok {
		if meta, data, ok := strings.Cut(rest, ","); ok {
			return block{Type: "image", Source: &imageSource{Type: "base64", MediaType: strings.TrimSuffix(meta, ";base64"), Data: data}}
		}
	}
	return block{Type: "image", Source: &imageSource{Type: "url", URL: u}}
}

// cloneSchema returns a tool's parameter schema as an object schema the request can edit (nil: no parameters).
func cloneSchema(in map[string]any) map[string]any {
	params := map[string]any{"type": "object", "properties": map[string]any{}}
	if len(in) == 0 {
		return params
	}
	b, err := json.Marshal(in)
	if err != nil {
		return params
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil || m == nil {
		return params
	}
	return m
}
