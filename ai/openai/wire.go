package openai

import (
	"encoding/json"

	"github.com/GhaziBenDahmane/harness/ai"
)

type wireMessage struct {
	Role             string         `json:"role"`
	Content          any            `json:"content"`
	Name             string         `json:"name,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type wirePart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *wireImageURL `json:"image_url,omitempty"`
}

type wireImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

func toWire(msgs []*ai.Message) []wireMessage {
	out := make([]wireMessage, 0, len(msgs))
	for _, m := range msgs {
		if m == nil {
			continue
		}
		w := wireMessage{Role: string(m.Role), Name: m.Name, ReasoningContent: m.ReasoningContent, ToolCallID: m.ToolCallID}
		w.Content = content(m)
		for _, c := range m.ToolCalls {
			typ := c.Type
			if typ == "" {
				typ = "function"
			}
			w.ToolCalls = append(w.ToolCalls, wireToolCall{ID: c.ID, Type: typ, Function: wireFunction{Name: c.Function.Name, Arguments: c.Function.Arguments}})
		}
		out = append(out, w)
	}
	return out
}

// content is the text of the message, or its parts when it carries images.
func content(m *ai.Message) any {
	if m.Role != ai.RoleUser || len(m.Parts) == 0 {
		return m.Content
	}
	parts := make([]wirePart, 0, len(m.Parts))
	for _, p := range m.Parts {
		switch p.Type {
		case ai.PartText:
			parts = append(parts, wirePart{Type: "text", Text: p.Text})
		case ai.PartImage:
			if p.Image == nil {
				continue
			}
			if u := imageURL(p.Image); u != "" {
				parts = append(parts, wirePart{Type: "image_url", ImageURL: &wireImageURL{URL: u, Detail: string(p.Image.Detail)}})
			}
		}
	}
	return parts
}

func imageURL(i *ai.Image) string {
	if i.URL != "" {
		return i.URL
	}
	if i.Base64Data != "" {
		mime := i.MIMEType
		if mime == "" {
			mime = "image/png"
		}
		return "data:" + mime + ";base64," + i.Base64Data
	}
	return ""
}

type wireUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (u *wireUsage) toUsage() *ai.TokenUsage {
	if u == nil {
		return nil
	}
	return &ai.TokenUsage{
		PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens,
		PromptTokenDetails:     ai.PromptTokenDetails{CachedTokens: u.PromptTokensDetails.CachedTokens},
		CompletionTokenDetails: ai.CompletionTokenDetails{ReasoningTokens: u.CompletionTokensDetails.ReasoningTokens},
	}
}

type wireChoiceMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content"`
	ToolCalls        []wireToolCall `json:"tool_calls"`
}

func (w wireChoiceMessage) toMessage() *ai.Message {
	m := &ai.Message{Role: ai.RoleAssistant, Content: w.Content, ReasoningContent: w.ReasoningContent}
	if w.Role != "" {
		m.Role = ai.Role(w.Role)
	}
	for _, c := range w.ToolCalls {
		m.ToolCalls = append(m.ToolCalls, ai.ToolCall{Index: c.Index, ID: c.ID, Type: c.Type,
			Function: ai.FunctionCall{Name: c.Function.Name, Arguments: c.Function.Arguments}})
	}
	return m
}

// parseCompletion reads a non-streamed answer.
func parseCompletion(raw []byte) (*ai.Message, error) {
	var env struct {
		Choices []struct {
			Message      wireChoiceMessage `json:"message"`
			FinishReason string            `json:"finish_reason"`
		} `json:"choices"`
		Usage *wireUsage `json:"usage"`
		Error *errorBody `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if env.Error != nil {
		return nil, &APIError{Code: env.Error.code(), Type: env.Error.Type, Message: env.Error.Message}
	}
	if len(env.Choices) == 0 {
		return nil, &APIError{Message: "the answer has no choices"}
	}
	m := env.Choices[0].Message.toMessage()
	m.ResponseMeta = &ai.ResponseMeta{FinishReason: env.Choices[0].FinishReason, Usage: env.Usage.toUsage()}
	return m, nil
}
