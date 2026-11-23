package executors

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Summary is what a parser learned from the output of a CLI.
type Summary struct {
	// Text is the CLI's final message.
	Text string
	// Tokens and USD are the usage the CLI reported (zero when it did not).
	Tokens int64
	USD    float64
	// Failed is set when the CLI said it failed; Err says why.
	Failed bool
	Err    string
}

// Parser reads a CLI's output line by line.
type Parser interface {
	// Line takes one line of output and returns the text to show as progress ("" shows nothing).
	Line(line string) string
	// Summary is what the output said, once it ended.
	Summary() Summary
}

// NewParser returns the parser called name (ParserText for an unknown one).
func NewParser(name string) Parser {
	switch name {
	case ParserClaudeJSON:
		return &claudeParser{}
	case ParserCodexJSONL:
		return &codexParser{}
	}
	return &textParser{}
}

// maxSummary bounds a summary kept from plain text.
const maxSummary = 6000

type textParser struct{ tail []string }

func (p *textParser) Line(line string) string {
	line = strings.TrimRight(line, "\r")
	p.tail = append(p.tail, line)
	if len(p.tail) > 200 {
		p.tail = p.tail[len(p.tail)-200:]
	}
	return line
}

func (p *textParser) Summary() Summary {
	s := strings.TrimSpace(strings.Join(p.tail, "\n"))
	if r := []rune(s); len(r) > maxSummary {
		s = "…" + string(r[len(r)-maxSummary:])
	}
	return Summary{Text: s}
}

// claudeParser reads `claude -p --output-format stream-json --verbose`: one JSON object per line, assistant
// messages with text and tool_use blocks, and a final "result" object with the answer and the cost.
type claudeParser struct {
	last Summary
	text string
	seen bool
}

func (p *claudeParser) Line(line string) string {
	var ev struct {
		Type     string  `json:"type"`
		Subtype  string  `json:"subtype"`
		Result   string  `json:"result"`
		IsError  bool    `json:"is_error"`
		Cost     float64 `json:"total_cost_usd"`
		RawUsage struct {
			In         int64 `json:"input_tokens"`
			Out        int64 `json:"output_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &ev); err != nil {
		return line // not JSON (a warning of the CLI): show it as it is
	}
	switch ev.Type {
	case "assistant":
		var out []string
		for _, c := range ev.Message.Content {
			switch c.Type {
			case "text":
				if t := strings.TrimSpace(c.Text); t != "" {
					out = append(out, t)
					p.text = t
				}
			case "tool_use":
				out = append(out, "[tool "+c.Name+"] "+toolHint(c.Input))
			}
		}
		return strings.Join(out, "\n")
	case "result":
		p.seen = true
		p.last = Summary{Text: strings.TrimSpace(ev.Result), USD: ev.Cost,
			Tokens: ev.RawUsage.In + ev.RawUsage.Out + ev.RawUsage.CacheRead + ev.RawUsage.CacheWrite}
		if ev.IsError || strings.HasPrefix(ev.Subtype, "error") {
			p.last.Failed = true
			p.last.Err = ev.Subtype
			if p.last.Text != "" {
				p.last.Err = p.last.Text
			}
		}
		return fmt.Sprintf("[done] %s", ev.Subtype)
	}
	return ""
}

func (p *claudeParser) Summary() Summary {
	s := p.last
	if !p.seen {
		s.Text = p.text
	}
	if s.Text == "" {
		s.Text = p.text
	}
	return s
}

// toolHint is a short view of a tool call's input: its file path or command.
func toolHint(raw json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	for _, k := range []string{"file_path", "path", "command", "pattern", "description"} {
		if v, ok := in[k].(string); ok && v != "" {
			if r := []rune(v); len(r) > 160 {
				v = string(r[:160]) + "…"
			}
			return v
		}
	}
	return ""
}

// codexParser reads `codex exec --json`: thread, turn and item events; the last agent_message is the answer and
// turn.completed carries the usage.
type codexParser struct {
	sum  Summary
	text string
}

func (p *codexParser) Line(line string) string {
	var ev struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
		Item struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Command  string `json:"command"`
			ExitCode *int   `json:"exit_code"`
		} `json:"item"`
		Usage struct {
			In     int64 `json:"input_tokens"`
			Cached int64 `json:"cached_input_tokens"`
			Out    int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &ev); err != nil {
		return line
	}
	switch ev.Type {
	case "item.completed", "item.started":
		if ev.Type == "item.started" {
			if ev.Item.Type == "command_execution" {
				return "[run] " + ev.Item.Command
			}
			return ""
		}
		switch ev.Item.Type {
		case "agent_message":
			p.text = strings.TrimSpace(ev.Item.Text)
			return p.text
		case "reasoning":
			return ""
		case "command_execution":
			return ""
		}
		return "[" + ev.Item.Type + "]"
	case "turn.completed":
		p.sum.Tokens += ev.Usage.In + ev.Usage.Out
		return ""
	case "turn.failed", "error":
		p.sum.Failed = true
		msg := ev.Message
		if msg == "" {
			msg = ev.Error.Message
		}
		p.sum.Err = msg
		return "[error] " + msg
	}
	return ""
}

func (p *codexParser) Summary() Summary {
	s := p.sum
	s.Text = p.text
	return s
}

var (
	reANSI = regexp.MustCompile("\x1b(?:\\[[0-?]*[ -/]*[@-~]|\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|[@-Z\\\\-_])")
)

// StripANSI removes terminal colour codes.
func StripANSI(s string) string { return reANSI.ReplaceAllString(s, "") }
