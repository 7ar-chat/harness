package anthropic

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/GhaziBenDahmane/harness/ai"
)

type usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type streamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage usage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		Thinking    string `json:"thinking"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *usage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type streamState struct {
	ctx   context.Context
	sw    *ai.StreamWriter[*ai.Message]
	u     usage
	calls map[int]int // content block index → tool call index
	n     int         // tool calls so far
	stop  string
	done  bool
}

func (s *streamState) send(m *ai.Message) bool { return !s.sw.Send(m, nil) }

func (s *streamState) run(body io.Reader) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		if s.ctx.Err() != nil {
			s.sw.Send(nil, s.ctx.Err())
			return
		}
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &ev); err != nil {
			continue
		}
		if !s.dispatch(ev) {
			return
		}
	}
	if err := sc.Err(); err != nil {
		s.sw.Send(nil, err)
		return
	}
	if !s.done {
		s.sw.Send(nil, io.ErrUnexpectedEOF)
	}
}

// dispatch handles one event; false ends the stream.
func (s *streamState) dispatch(ev streamEvent) bool {
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			s.u = ev.Message.Usage
		}
	case "content_block_start":
		if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
			if s.calls == nil {
				s.calls = map[int]int{}
			}
			s.calls[ev.Index] = s.n
			s.n++
			return s.send(callChunk(s.calls[ev.Index], ev.ContentBlock.ID, ev.ContentBlock.Name, ""))
		}
	case "content_block_delta":
		if ev.Delta == nil {
			return true
		}
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text != "" {
				return s.send(&ai.Message{Role: ai.RoleAssistant, Content: ev.Delta.Text})
			}
		case "thinking_delta":
			if ev.Delta.Thinking != "" {
				return s.send(&ai.Message{Role: ai.RoleAssistant, ReasoningContent: ev.Delta.Thinking})
			}
		case "input_json_delta":
			if idx, ok := s.calls[ev.Index]; ok && ev.Delta.PartialJSON != "" {
				return s.send(callChunk(idx, "", "", ev.Delta.PartialJSON))
			}
		}
	case "message_delta":
		if ev.Delta != nil {
			s.stop = ev.Delta.StopReason
		}
		if ev.Usage != nil {
			s.u.OutputTokens = ev.Usage.OutputTokens
			if ev.Usage.InputTokens > 0 { // some proxies repeat the final input counts here
				s.u.InputTokens = ev.Usage.InputTokens
				s.u.CacheReadInputTokens = ev.Usage.CacheReadInputTokens
				s.u.CacheCreationInputTokens = ev.Usage.CacheCreationInputTokens
			}
		}
	case "message_stop":
		s.done = true
		s.send(s.final())
		return false
	case "error":
		s.done = true
		e := &APIError{Status: 500}
		if ev.Error != nil {
			e.Type, e.Message, e.Status = ev.Error.Type, ev.Error.Message, statusOfType(ev.Error.Type)
		}
		s.sw.Send(nil, e)
		return false
	}
	return true
}

// final is the closing chunk: finish reason and usage. Anthropic's input_tokens
// excludes the cached parts, so the prompt size is the three summed.
func (s *streamState) final() *ai.Message {
	reason := "stop"
	switch s.stop {
	case "tool_use":
		reason = "tool_calls"
	case "max_tokens", "model_context_window_exceeded":
		reason = "length"
	case "refusal":
		reason = "content_filter"
	}
	prompt := s.u.InputTokens + s.u.CacheReadInputTokens + s.u.CacheCreationInputTokens
	return &ai.Message{Role: ai.RoleAssistant, ResponseMeta: &ai.ResponseMeta{
		FinishReason: reason,
		Usage: &ai.TokenUsage{
			PromptTokens: prompt,
			PromptTokenDetails: ai.PromptTokenDetails{
				CachedTokens: s.u.CacheReadInputTokens, CacheWriteTokens: s.u.CacheCreationInputTokens},
			CompletionTokens: s.u.OutputTokens,
			TotalTokens:      prompt + s.u.OutputTokens,
		},
	}}
}

func callChunk(index int, id, name, args string) *ai.Message {
	i := index
	return &ai.Message{Role: ai.RoleAssistant, ToolCalls: []ai.ToolCall{{Index: &i, ID: id, Type: "function", Function: ai.FunctionCall{Name: name, Arguments: args}}}}
}
