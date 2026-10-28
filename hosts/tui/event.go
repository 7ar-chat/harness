// Package tui is a terminal client of the harness: a Bubble Tea model fed by
// the AG-UI events of a run, pure render functions for each event type, a
// print mode for scripts, and the Backend that connects them to a
// client.Client (in process or remote). It imports only hosts/client and
// engine/agui.
package tui

import (
	"encoding/json"
	"strings"

	"github.com/GhaziBenDahmane/harness/engine/agui"
	"github.com/GhaziBenDahmane/harness/hosts/client"
)

// Ev is a decoded AG-UI event: only the fields the terminal client uses.
type Ev struct {
	Type string
	Seq  int64
	Raw  json.RawMessage

	// Text and reasoning messages.
	MessageID string
	Delta     string

	// Tool calls.
	ToolCallID string
	ToolName   string
	Content    string // TOOL_CALL_RESULT
	IsError    bool

	// STEP_STARTED / STEP_FINISHED.
	StepName string

	// CUSTOM.
	Name  string
	Value map[string]any

	// RUN_FINISHED.
	Outcome    string
	Interrupts []agui.Interrupt
	Usage      []agui.Usage

	// RUN_ERROR.
	Code    string
	Message string
}

// Decode parses a stored event; an undecodable payload yields an Ev with only
// Type and Seq set.
func Decode(se client.Event) Ev {
	ev := Ev{Type: se.Type, Seq: se.Seq, Raw: se.Data}
	var w struct {
		MessageID    string         `json:"messageId"`
		Delta        string         `json:"delta"`
		ToolCallID   string         `json:"toolCallId"`
		ToolCallName string         `json:"toolCallName"`
		Content      string         `json:"content"`
		Metadata     map[string]any `json:"metadata"`
		StepName     string         `json:"stepName"`
		Name         string         `json:"name"`
		Value        map[string]any `json:"value"`
		Outcome      struct {
			Type       string           `json:"type"`
			Interrupts []agui.Interrupt `json:"interrupts"`
		} `json:"outcome"`
		Usage   []agui.Usage `json:"usage"`
		Code    string       `json:"code"`
		Message string       `json:"message"`
		Type    string       `json:"type"`
	}
	if json.Unmarshal(se.Data, &w) != nil {
		return ev
	}
	if ev.Type == "" {
		ev.Type = w.Type
	}
	ev.MessageID, ev.Delta, ev.ToolCallID, ev.ToolName = w.MessageID, w.Delta, w.ToolCallID, w.ToolCallName
	ev.Content, ev.StepName, ev.Name, ev.Value = w.Content, w.StepName, w.Name, w.Value
	ev.Outcome, ev.Interrupts, ev.Usage = w.Outcome.Type, w.Outcome.Interrupts, w.Usage
	ev.Code, ev.Message = w.Code, w.Message
	ev.IsError, _ = w.Metadata["isError"].(bool)
	return ev
}

// Terminal reports whether the event ends the run's stream.
func (e Ev) Terminal() bool { return agui.IsTerminal(e.Type) }

// UsageValue reads a work.usage CUSTOM value.
func (e Ev) UsageValue() Usage {
	n := func(k string) int { f, _ := e.Value[k].(float64); return int(f) }
	return Usage{In: n("inputTokens"), Out: n("outputTokens"), Cached: n("cachedInputTokens"), Reasoning: n("reasoningTokens"),
		Context: n("contextTokens"), Window: n("contextWindow")}
}

// Usage is a token count.
type Usage struct {
	In, Out, Cached, Reasoning int
	Context, Window            int
}

// SumUsage totals the per-model usage of a RUN_FINISHED.
func SumUsage(us []agui.Usage) Usage {
	var t Usage
	for _, u := range us {
		t.In += u.InputTokens
		t.Out += u.OutputTokens
		t.Cached += u.CachedInputTokens
		t.Reasoning += u.ReasoningTokens
	}
	return t
}

// CachePercent is the share of input tokens served from the prompt cache.
func (u Usage) CachePercent() int {
	if u.In <= 0 {
		return 0
	}
	return u.Cached * 100 / u.In
}

// argString reads a string field of a tool call's JSON arguments.
func argString(args, key string) string {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// summaryKeys are the argument names worth showing in a tool call's header,
// in order of preference.
var summaryKeys = []string{"path", "command", "cmd", "pattern", "query", "url", "name", "file", "skill", "question"}

// ToolSummary is the key argument of a tool call (path, command…), one line.
func ToolSummary(args string) string {
	for _, k := range summaryKeys {
		if s := argString(args, k); s != "" {
			return oneLine(s)
		}
	}
	return ""
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

// clip shortens s to n runes.
func clip(s string, n int) string {
	r := []rune(s)
	if n <= 0 || len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}
