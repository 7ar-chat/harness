package ai

import (
	"fmt"
	"sort"
	"strings"
)

// ConcatMessages merges the chunks of one streamed answer into a message: text and reasoning are
// appended, tool-call fragments with the same Index are joined (arguments concatenated), token usage
// keeps the largest figure of each counter and the last finish reason wins. Chunks must agree on role,
// name and tool-call id; Extra values merge key by key (strings append, a single value is kept).
func ConcatMessages(msgs []*Message) (*Message, error) {
	var (
		ret       Message
		content   strings.Builder
		reasoning strings.Builder
		calls     []ToolCall
		extras    []map[string]any
	)
	for i, m := range msgs {
		if m == nil {
			return nil, fmt.Errorf("unexpected nil chunk in message stream, index: %d", i)
		}
		role, err := same("roles", string(ret.Role), string(m.Role))
		if err != nil {
			return nil, err
		}
		ret.Role = Role(role)
		if ret.Name, err = same("names", ret.Name, m.Name); err != nil {
			return nil, err
		}
		if ret.ToolCallID, err = same("toolCallIDs", ret.ToolCallID, m.ToolCallID); err != nil {
			return nil, err
		}
		if ret.ToolName, err = same("toolNames", ret.ToolName, m.ToolName); err != nil {
			return nil, err
		}
		content.WriteString(m.Content)
		reasoning.WriteString(m.ReasoningContent)
		calls = append(calls, m.ToolCalls...)
		if len(m.Extra) > 0 {
			extras = append(extras, m.Extra)
		}
		ret.Parts = append(ret.Parts, m.Parts...)
		if m.ResponseMeta != nil {
			if ret.ResponseMeta == nil {
				ret.ResponseMeta = &ResponseMeta{}
			}
			mergeMeta(ret.ResponseMeta, m.ResponseMeta)
		}
	}
	ret.Content, ret.ReasoningContent = content.String(), reasoning.String()
	if len(calls) > 0 {
		merged, err := concatToolCalls(calls)
		if err != nil {
			return nil, err
		}
		ret.ToolCalls = merged
	}
	if len(extras) > 0 {
		extra, err := concatExtra(extras)
		if err != nil {
			return nil, fmt.Errorf("failed to concat message's extra: %w", err)
		}
		ret.Extra = extra
	}
	ret.Parts = mergeTextParts(ret.Parts)
	return &ret, nil
}

// same returns the value two chunks agree on; an empty one defers to the other.
func same(what, have, got string) (string, error) {
	switch {
	case got == "":
		return have, nil
	case have == "" || have == got:
		return got, nil
	}
	return "", fmt.Errorf("cannot concat messages with different %s: '%s' '%s'", what, have, got)
}

func mergeMeta(dst, src *ResponseMeta) {
	if src.FinishReason != "" {
		dst.FinishReason = src.FinishReason
	}
	if src.Usage == nil {
		return
	}
	if dst.Usage == nil {
		dst.Usage = &TokenUsage{}
	}
	d, s := dst.Usage, src.Usage
	d.PromptTokens = max(d.PromptTokens, s.PromptTokens)
	d.CompletionTokens = max(d.CompletionTokens, s.CompletionTokens)
	d.TotalTokens = max(d.TotalTokens, s.TotalTokens)
	d.PromptTokenDetails.CachedTokens = max(d.PromptTokenDetails.CachedTokens, s.PromptTokenDetails.CachedTokens)
	d.PromptTokenDetails.CacheWriteTokens = max(d.PromptTokenDetails.CacheWriteTokens, s.PromptTokenDetails.CacheWriteTokens)
	d.CompletionTokenDetails.ReasoningTokens = max(d.CompletionTokenDetails.ReasoningTokens, s.CompletionTokenDetails.ReasoningTokens)
}

// concatToolCalls joins the fragments that share an Index; calls without an Index are kept whole and
// come first, the indexed ones follow in index order.
func concatToolCalls(chunks []ToolCall) ([]ToolCall, error) {
	var merged []ToolCall
	byIndex := map[int][]int{}
	for i := range chunks {
		if chunks[i].Index == nil {
			merged = append(merged, chunks[i])
			continue
		}
		byIndex[*chunks[i].Index] = append(byIndex[*chunks[i].Index], i)
	}
	for idx, group := range byIndex {
		index := idx
		call := chunks[group[0]]
		call.Index = &index
		var id, typ, name string
		var args strings.Builder
		for _, n := range group {
			c := chunks[n]
			var err error
			if id, err = sameCall("id", id, c.ID); err != nil {
				return nil, err
			}
			if typ, err = sameCall("type", typ, c.Type); err != nil {
				return nil, err
			}
			if name, err = sameCall("name", name, c.Function.Name); err != nil {
				return nil, err
			}
			args.WriteString(c.Function.Arguments)
		}
		call.ID, call.Type, call.Function = id, typ, FunctionCall{Name: name, Arguments: args.String()}
		merged = append(merged, call)
	}
	sort.SliceStable(merged, func(i, j int) bool {
		a, b := merged[i].Index, merged[j].Index
		switch {
		case a == nil:
			return b != nil
		case b == nil:
			return false
		}
		return *a < *b
	})
	return merged, nil
}

func sameCall(what, have, got string) (string, error) {
	switch {
	case got == "":
		return have, nil
	case have == "" || have == got:
		return got, nil
	}
	return "", fmt.Errorf("cannot concat ToolCalls with different tool %s: '%s' '%s'", what, have, got)
}

// mergeTextParts joins runs of adjacent text parts.
func mergeTextParts(parts []Part) []Part {
	if len(parts) == 0 {
		return nil
	}
	out := make([]Part, 0, len(parts))
	for i := 0; i < len(parts); {
		if parts[i].Type != PartText {
			out = append(out, parts[i])
			i++
			continue
		}
		j := i + 1
		for j < len(parts) && parts[j].Type == PartText {
			j++
		}
		if j == i+1 {
			out = append(out, parts[i])
		} else {
			var b strings.Builder
			for _, p := range parts[i:j] {
				b.WriteString(p.Text)
			}
			out = append(out, Part{Type: PartText, Text: b.String()})
		}
		i = j
	}
	return out
}

// concatExtra merges the Extra maps of a message's chunks: a key seen once keeps its value, string
// values seen several times are appended, any other repeated value is an error.
func concatExtra(list []map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for _, m := range list {
		for k, v := range m {
			prev, ok := out[k]
			if !ok {
				out[k] = v
				continue
			}
			ps, pok := prev.(string)
			vs, vok := v.(string)
			if !pok || !vok {
				return nil, fmt.Errorf("cannot concat multiple values of key %q (%T, %T)", k, prev, v)
			}
			out[k] = ps + vs
		}
	}
	return out, nil
}
