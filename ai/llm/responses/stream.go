package responses

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/7ar-chat/harness/ai"
)

// ErrStreamEnded is the cause of a stream that ended before its terminal event
// (it wraps io.ErrUnexpectedEOF: a transient failure, the step is retried).
var ErrStreamEnded = fmt.Errorf("responses: the stream ended before response.completed: %w", io.ErrUnexpectedEOF)

type outputItem struct {
	ID               string        `json:"id"`
	Type             string        `json:"type"`
	Status           string        `json:"status"`
	CallID           string        `json:"call_id"`
	Name             string        `json:"name"`
	Arguments        string        `json:"arguments"`
	Role             string        `json:"role"`
	EncryptedContent string        `json:"encrypted_content"`
	Summary          []SummaryPart `json:"summary"`
	Content          []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
}

type usage struct {
	InputTokens        int `json:"input_tokens"`
	InputTokensDetails struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens        int `json:"output_tokens"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	TotalTokens int `json:"total_tokens"`
}

type responseObject struct {
	Status            string     `json:"status"`
	Error             *errorBody `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage  *usage       `json:"usage"`
	Output []outputItem `json:"output"`
}

type event struct {
	Type         string          `json:"type"`
	OutputIndex  int             `json:"output_index"`
	ItemID       string          `json:"item_id"`
	SummaryIndex int             `json:"summary_index"`
	Delta        string          `json:"delta"`
	Arguments    string          `json:"arguments"`
	Item         *outputItem     `json:"item"`
	Response     *responseObject `json:"response"`
	// error events: {"type":"error","code","message"} (or nested in "error").
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
	Error   *errorBody      `json:"error"`
}

type liveCall struct {
	index   int
	hasArgs bool
}

// streamState maps the events of one response to ai.Message chunks:
// reasoning summary deltas → ReasoningContent, output_text deltas → Content,
// function calls → ToolCalls with a stable Index (the call's rank) and the
// call_id on their first fragment, the terminal event → ResponseMeta (usage,
// finish_reason: stop, tool_calls, length, content_filter) and the reasoning
// items (ExtraReasoningItems).
type streamState struct {
	cfg   *Config
	ctx   context.Context
	sw    *ai.StreamWriter[*ai.Message]
	calls map[string]*liveCall // by item id
	byOut map[int]*liveCall    // by output_index
	n     int
	texts map[string]bool // message items that streamed text
	parts map[string]int  // reasoning item → summary parts seen
	items []ReasoningItem
}

func (s *streamState) send(m *ai.Message) bool {
	m.Role = ai.RoleAssistant
	return !s.sw.Send(m, nil)
}

func (s *streamState) fail(err error) { s.sw.Send(nil, err) }

func (s *streamState) run(body io.Reader) {
	s.calls, s.byOut, s.texts, s.parts = map[string]*liveCall{}, map[int]*liveCall{}, map[string]bool{}, map[string]int{}
	r := bufio.NewReaderSize(body, 64<<10)
	var data bytes.Buffer
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			switch {
			case len(line) == 0:
				if data.Len() > 0 {
					done, ok := s.dispatch(data.Bytes())
					data.Reset()
					if done || !ok {
						return
					}
				}
			case bytes.HasPrefix(line, []byte("data:")):
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.Write(bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" ")))
			}
		}
		if err != nil {
			if data.Len() > 0 {
				if done, ok := s.dispatch(data.Bytes()); done || !ok {
					return
				}
			}
			if errors.Is(err, io.EOF) {
				s.fail(ErrStreamEnded)
			} else {
				s.fail(err)
			}
			return
		}
	}
}

// dispatch handles one SSE event: done after the terminal event, !ok when the reader went away.
func (s *streamState) dispatch(data []byte) (done, ok bool) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return false, true
	}
	var ev event
	if err := json.Unmarshal(data, &ev); err != nil {
		return false, true // unknown framing: skip it
	}
	switch ev.Type {
	case "response.reasoning_summary_text.delta":
		if ev.Delta == "" {
			return false, true
		}
		delta := ev.Delta
		if seen, started := s.parts[ev.ItemID]; started && ev.SummaryIndex > seen {
			delta = "\n\n" + delta
		}
		s.parts[ev.ItemID] = ev.SummaryIndex
		return false, s.send(&ai.Message{ReasoningContent: delta})
	case "response.output_text.delta":
		if ev.Delta == "" {
			return false, true
		}
		s.texts[ev.ItemID] = true
		return false, s.send(&ai.Message{Content: ev.Delta})
	case "response.refusal.delta":
		if s.cfg.OnRefusal != nil && ev.Delta != "" {
			s.cfg.OnRefusal(s.ctx, ev.Delta)
		}
	case "response.output_item.added":
		if ev.Item != nil && ev.Item.Type == "function_call" {
			c := &liveCall{index: s.n}
			s.n++
			s.calls[ev.Item.ID], s.byOut[ev.OutputIndex] = c, c
			c.hasArgs = ev.Item.Arguments != ""
			return false, s.send(callChunk(c.index, ev.Item.CallID, ev.Item.Name, ev.Item.Arguments))
		}
	case "response.function_call_arguments.delta":
		if c := s.call(ev); c != nil && ev.Delta != "" {
			c.hasArgs = true
			return false, s.send(callChunk(c.index, "", "", ev.Delta))
		}
	case "response.function_call_arguments.done":
		if c := s.call(ev); c != nil && !c.hasArgs && ev.Arguments != "" {
			c.hasArgs = true
			return false, s.send(callChunk(c.index, "", "", ev.Arguments))
		}
	case "response.output_item.done":
		return false, s.itemDone(ev)
	case "response.completed", "response.incomplete":
		return true, s.finish(ev.Response, ev.Type == "response.incomplete")
	case "response.failed":
		if r := ev.Response; r != nil && r.Error != nil {
			if statusOfCode(r.Error.code(), r.Error.Type) == 400 && containsFold(r.Error.code(), "content_filter") {
				return true, s.finishWith("content_filter", r.Usage)
			}
			s.fail(streamError(r.Error.code(), r.Error.Type, r.Error.Message))
		} else {
			s.fail(streamError("server_error", "", "the response failed"))
		}
		return true, false
	case "error":
		b := errorBody{Code: ev.Code, Message: ev.Message}
		if ev.Error != nil {
			b = *ev.Error
		}
		s.fail(streamError(b.code(), b.Type, b.Message))
		return true, false
	}
	return false, true
}

func (s *streamState) call(ev event) *liveCall {
	if c := s.calls[ev.ItemID]; c != nil {
		return c
	}
	return s.byOut[ev.OutputIndex]
}

func (s *streamState) itemDone(ev event) bool {
	it := ev.Item
	if it == nil {
		return true
	}
	switch it.Type {
	case "reasoning":
		s.addItem(*it)
	case "function_call":
		c := s.calls[it.ID]
		if c == nil {
			c = s.byOut[ev.OutputIndex]
		}
		if c == nil { // never announced: announce it whole
			c = &liveCall{index: s.n}
			s.n++
			s.calls[it.ID] = c
			c.hasArgs = true
			return s.send(callChunk(c.index, it.CallID, it.Name, it.Arguments))
		}
		if !c.hasArgs && it.Arguments != "" {
			c.hasArgs = true
			return s.send(callChunk(c.index, "", "", it.Arguments))
		}
	case "message":
		if s.texts[it.ID] {
			return true
		}
		for _, p := range it.Content {
			if p.Type == "output_text" && p.Text != "" {
				s.texts[it.ID] = true
				if !s.send(&ai.Message{Content: p.Text}) {
					return false
				}
			}
		}
	}
	return true
}

func (s *streamState) addItem(it outputItem) {
	if it.ID == "" || it.EncryptedContent == "" {
		return
	}
	for _, x := range s.items {
		if x.ID == it.ID {
			return
		}
	}
	s.items = append(s.items, ReasoningItem{ID: it.ID, EncryptedContent: it.EncryptedContent, Summary: it.Summary, Model: s.cfg.Name})
}

func (s *streamState) finish(r *responseObject, incomplete bool) bool {
	if r == nil {
		r = &responseObject{}
	}
	if len(s.items) == 0 {
		for _, it := range r.Output {
			if it.Type == "reasoning" {
				s.addItem(it)
			}
		}
	}
	reason := "stop"
	if s.n > 0 {
		reason = "tool_calls"
	}
	if incomplete {
		reason = "length"
		if d := r.IncompleteDetails; d != nil && containsFold(d.Reason, "content_filter") {
			reason = "content_filter"
		}
	}
	return s.finishWith(reason, r.Usage)
}

func (s *streamState) finishWith(reason string, u *usage) bool {
	msg := &ai.Message{ResponseMeta: &ai.ResponseMeta{FinishReason: reason}}
	if u != nil {
		msg.ResponseMeta.Usage = &ai.TokenUsage{
			PromptTokens: u.InputTokens,
			PromptTokenDetails: ai.PromptTokenDetails{CachedTokens: u.InputTokensDetails.CachedTokens,
				CacheWriteTokens: u.InputTokensDetails.CacheWriteTokens},
			CompletionTokens:       u.OutputTokens,
			TotalTokens:            u.TotalTokens,
			CompletionTokenDetails: ai.CompletionTokenDetails{ReasoningTokens: u.OutputTokensDetails.ReasoningTokens},
		}
	}
	SetItems(msg, s.items)
	return s.send(msg)
}

func callChunk(index int, id, name, args string) *ai.Message {
	i := index
	return &ai.Message{ToolCalls: []ai.ToolCall{{Index: &i, ID: id, Type: "function", Function: ai.FunctionCall{Name: name, Arguments: args}}}}
}

func containsFold(s, sub string) bool {
	return bytes.Contains(bytes.ToLower([]byte(s)), []byte(sub))
}
