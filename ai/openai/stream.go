package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/GhaziBenDahmane/harness/ai"
)

type chunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string         `json:"role"`
			Content          string         `json:"content"`
			ReasoningContent string         `json:"reasoning_content"`
			ToolCalls        []wireToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
	Error *errorBody `json:"error"`
}

// readStream sends one message per server-sent event, then closes by returning (the caller closes sw).
func readStream(ctx context.Context, body io.Reader, sw *ai.StreamWriter[*ai.Message]) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	done, finished := false, false
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			sw.Send(nil, err)
			return
		}
		line := bytes.TrimSpace(sc.Bytes())
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		data = bytes.TrimSpace(data)
		if len(data) == 0 {
			continue
		}
		if string(data) == "[DONE]" {
			done = true
			break
		}
		var c chunk
		if err := json.Unmarshal(data, &c); err != nil {
			sw.Send(nil, err)
			return
		}
		if c.Error != nil {
			sw.Send(nil, &APIError{Code: c.Error.code(), Type: c.Error.Type, Message: c.Error.Message})
			return
		}
		msg := &ai.Message{Role: ai.RoleAssistant}
		meta := false
		for _, ch := range c.Choices {
			if ch.Index != 0 {
				continue
			}
			msg.Content, msg.ReasoningContent = ch.Delta.Content, ch.Delta.ReasoningContent
			for _, tc := range ch.Delta.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, ai.ToolCall{Index: tc.Index, ID: tc.ID, Type: tc.Type,
					Function: ai.FunctionCall{Name: tc.Function.Name, Arguments: tc.Function.Arguments}})
			}
			if ch.FinishReason != "" {
				msg.ResponseMeta, meta, finished = &ai.ResponseMeta{FinishReason: ch.FinishReason}, true, true
			}
		}
		if c.Usage != nil {
			if msg.ResponseMeta == nil {
				msg.ResponseMeta = &ai.ResponseMeta{}
			}
			msg.ResponseMeta.Usage, meta = c.Usage.toUsage(), true
		}
		if msg.Content == "" && msg.ReasoningContent == "" && len(msg.ToolCalls) == 0 && !meta {
			continue
		}
		if sw.Send(msg, nil) {
			return
		}
	}
	if err := sc.Err(); err != nil {
		sw.Send(nil, err)
		return
	}
	if !done && !finished {
		sw.Send(nil, io.ErrUnexpectedEOF)
	}
}
