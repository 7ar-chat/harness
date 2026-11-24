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
	case ParserOMPJSON:
		return &ompParser{}
	}
	return &textParser{}
}

// maxSummary bounds a summary kept from plain text.
const maxSummary = 6000

// maxProgress bounds one progress line shown to the user.
const maxProgress = 400

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

// nonJSON is the progress shown for a line a JSON parser could not read: a warning of the CLI is shown as it is, but
// a huge fragment of an event the runner had to cut (a line over its 1 MiB limit) is not, it would flood the output.
func nonJSON(line string) string {
	if t := strings.TrimSpace(line); len(t) > 2000 && (t[0] == '{' || t[0] == '[') {
		return "[an oversized event was skipped]"
	}
	return line
}

// firstLine is the first non-empty line of s, clipped for progress.
func firstLine(s string, n int) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if r := []rune(l); len(r) > n {
				return string(r[:n]) + "…"
			}
			return l
		}
	}
	return ""
}

// lastLine is the last non-empty line of s, clipped for progress.
func lastLine(s string, n int) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	return firstLine(ls[len(ls)-1], n)
}

// rawText reads a JSON value that is a string or a list of {text} blocks.
func rawText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

// claudeParser reads `claude -p --output-format stream-json --verbose`, as the real CLI prints it (checked against
// Claude Code 2.1): one JSON object per line. A long system "init" line, one "assistant" line per content block
// (text, tool_use), "user" lines carrying tool results, system lines for retries and permission denials, and a final
// "result" object with the answer, the cost, the usage and the permission denials. A failed run (bad key, no login)
// still ends with a "result" whose is_error is true while its subtype says "success".
type claudeParser struct {
	last   Summary
	text   string
	seen   bool
	denied []string
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
		// Message is an object on assistant and user lines and a string on system lines.
		Message         json.RawMessage `json:"message"`
		ParentToolUseID *string         `json:"parent_tool_use_id"`
		ToolName        string          `json:"tool_name"`
		Attempt         int             `json:"attempt"`
		MaxRetries      int             `json:"max_retries"`
		ErrorStatus     *int            `json:"error_status"`
		Error           string          `json:"error"`
		TerminalReason  string          `json:"terminal_reason"`
		Denials         []struct {
			ToolName  string          `json:"tool_name"`
			ToolInput json.RawMessage `json:"tool_input"`
		} `json:"permission_denials"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &ev); err != nil {
		return nonJSON(line)
	}
	switch ev.Type {
	case "system":
		var msg string
		_ = json.Unmarshal(ev.Message, &msg)
		switch ev.Subtype {
		case "api_retry":
			why := ev.Error
			if ev.ErrorStatus != nil {
				why = fmt.Sprintf("%s (HTTP %d)", why, *ev.ErrorStatus)
			}
			return fmt.Sprintf("[retry %d/%d] %s", ev.Attempt, ev.MaxRetries, why)
		case "permission_denied":
			return "[denied] " + ev.ToolName + ": " + firstLine(msg, maxProgress)
		}
		return "" // init, informational, status, hooks
	case "assistant", "user":
		var m struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(ev.Message, &m) != nil {
			return ""
		}
		var blocks []struct {
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Name    string          `json:"name"`
			Input   json.RawMessage `json:"input"`
			IsError bool            `json:"is_error"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(m.Content, &blocks) != nil {
			return "" // a user message with plain string content
		}
		var out []string
		for _, c := range blocks {
			switch c.Type {
			case "text":
				if t := strings.TrimSpace(c.Text); t != "" {
					out = append(out, t)
					if ev.ParentToolUseID == nil { // a sub-agent's words are not the answer
						p.text = t
					}
				}
			case "tool_use":
				out = append(out, "[tool "+c.Name+"] "+toolHint(c.Input))
			case "tool_result":
				if c.IsError {
					out = append(out, "[tool error] "+firstLine(rawText(c.Content), maxProgress))
				}
			}
		}
		return strings.Join(out, "\n")
	case "result":
		p.seen = true
		p.last = Summary{Text: strings.TrimSpace(ev.Result), USD: ev.Cost,
			Tokens: ev.RawUsage.In + ev.RawUsage.Out + ev.RawUsage.CacheRead + ev.RawUsage.CacheWrite}
		for _, d := range ev.Denials {
			p.denied = append(p.denied, strings.TrimSpace(d.ToolName+" "+toolHint(d.ToolInput)))
		}
		if ev.IsError || strings.HasPrefix(ev.Subtype, "error") {
			p.last.Failed = true
			p.last.Err = ev.Subtype
			if p.last.Text != "" {
				p.last.Err = p.last.Text
			}
			return "[failed] " + p.last.Err
		}
		return fmt.Sprintf("[done] %s", ev.Subtype)
	}
	return ""
}

func (p *claudeParser) Summary() Summary {
	s := p.last
	if !p.seen || s.Text == "" {
		s.Text = p.text
	}
	if len(p.denied) > 0 && !s.Failed {
		// The CLI refused tool calls (read-only plan mode, a tool outside the allowed list): the answer alone would
		// read as if the work had been done.
		note := "(the CLI refused these tool calls: " + strings.Join(p.denied, "; ") + ")"
		if s.Text == "" {
			s.Text = note
		} else {
			s.Text += "\n\n" + note
		}
	}
	return s
}

// toolHint is a short view of a tool call's input: its file path or command.
func toolHint(raw json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	for _, k := range []string{"file_path", "path", "command", "cmd", "pattern", "url", "query", "description"} {
		if v, ok := in[k].(string); ok && v != "" {
			return firstLine(v, 160)
		}
	}
	return ""
}

// codexParser reads `codex exec --json` (checked against codex-cli 0.172): thread.started, turn.started, item.started
// and item.completed events (agent_message, command_execution, error notices, ...), transient "error" events while
// it retries a request ("Reconnecting... 2/5"), then turn.completed with the usage, or turn.failed. A transient
// error does NOT mean the run failed: a run that reconnected and then succeeded ends with turn.completed.
type codexParser struct {
	sum      Summary
	text     string
	lastErr  string
	done     bool
	failed   bool
	failedAt string
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
			Message  string `json:"message"`
			Command  string `json:"command"`
			Output   string `json:"aggregated_output"`
			ExitCode *int   `json:"exit_code"`
			Server   string `json:"server"`
			Tool     string `json:"tool"`
			Query    string `json:"query"`
			Changes  []struct {
				Path string `json:"path"`
				Kind string `json:"kind"`
			} `json:"changes"`
		} `json:"item"`
		Usage struct {
			In  int64 `json:"input_tokens"`
			Out int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &ev); err != nil {
		return nonJSON(line)
	}
	switch ev.Type {
	case "item.started":
		if ev.Item.Type == "command_execution" {
			return "[run] " + shellCommand(ev.Item.Command)
		}
	case "item.completed":
		it := ev.Item
		switch it.Type {
		case "agent_message":
			p.text = strings.TrimSpace(it.Text)
			return p.text
		case "command_execution":
			if it.ExitCode != nil && *it.ExitCode != 0 {
				out := fmt.Sprintf("[run failed, exit %d]", *it.ExitCode)
				if l := lastLine(it.Output, 200); l != "" {
					out += " " + l
				}
				return out
			}
		case "error":
			return "[warning] " + firstLine(it.Message, maxProgress)
		case "file_change":
			var parts []string
			for _, c := range it.Changes {
				parts = append(parts, c.Kind+" "+c.Path)
			}
			return "[edit] " + strings.Join(parts, ", ")
		case "mcp_tool_call":
			return "[tool " + it.Server + "." + it.Tool + "]"
		case "web_search":
			return "[search] " + it.Query
		}
	case "turn.completed":
		p.done = true
		p.failed = false
		p.sum.Tokens += ev.Usage.In + ev.Usage.Out
	case "turn.failed":
		p.failed = true
		p.failedAt = ev.Error.Message
		if p.failedAt == "" {
			p.failedAt = ev.Message
		}
		return "[error] " + firstLine(p.failedAt, maxProgress)
	case "error":
		p.lastErr = ev.Message
		if p.lastErr == "" {
			p.lastErr = ev.Error.Message
		}
		if strings.HasPrefix(p.lastErr, "Reconnecting") {
			return "[retry] " + firstLine(p.lastErr, maxProgress)
		}
		return "[error] " + firstLine(p.lastErr, maxProgress)
	}
	return ""
}

func (p *codexParser) Summary() Summary {
	s := p.sum
	s.Text = p.text
	switch {
	case p.failed:
		s.Failed, s.Err = true, p.failedAt
	case !p.done && p.lastErr != "":
		// The stream ended in an error without completing the turn.
		s.Failed, s.Err = true, p.lastErr
	}
	return s
}

var reShellWrap = regexp.MustCompile(`(?s)^(?:\S*/)?(?:ba|z|da)?sh -l?c (.*)$`)

// shellCommand shows the command Codex ran without its shell wrapper (/bin/zsh -lc "...").
func shellCommand(cmd string) string {
	if m := reShellWrap.FindStringSubmatch(cmd); m != nil {
		cmd = strings.TrimSpace(m[1])
		if len(cmd) >= 2 && (cmd[0] == '"' || cmd[0] == '\'') && cmd[len(cmd)-1] == cmd[0] {
			cmd = cmd[1 : len(cmd)-1]
		}
	}
	return firstLine(cmd, 160)
}

// ompParser reads `omp --mode json` (checked against omp 19.1): a session header, then one JSON event per line
// (agent_start, turn_start, message_start/update/end, tool_execution_start/end, turn_end, agent_end). The assistant
// message_end events carry the text, the usage ({input, output, cacheRead, cacheWrite, totalTokens, cost.total}) and
// the stop reason; a failed request ends the turn with stopReason "error" and an errorMessage (exit code 1).
type ompParser struct {
	sum     Summary
	text    string
	failed  bool
	errText string
}

func (p *ompParser) Line(line string) string {
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Usage struct {
				TotalTokens int64 `json:"totalTokens"`
				Input       int64 `json:"input"`
				Output      int64 `json:"output"`
				CacheRead   int64 `json:"cacheRead"`
				CacheWrite  int64 `json:"cacheWrite"`
				Cost        struct {
					Total float64 `json:"total"`
				} `json:"cost"`
			} `json:"usage"`
			StopReason   string `json:"stopReason"`
			ErrorMessage string `json:"errorMessage"`
		} `json:"message"`
		ToolName string          `json:"toolName"`
		Args     json.RawMessage `json:"args"`
		IsError  bool            `json:"isError"`
		Result   struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Attempt      int    `json:"attempt"`
		MaxAttempts  int    `json:"maxAttempts"`
		ErrorMessage string `json:"errorMessage"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &ev); err != nil {
		return nonJSON(line)
	}
	switch ev.Type {
	case "message_end":
		m := ev.Message
		if m.Role != "assistant" {
			return ""
		}
		u := m.Usage
		total := u.TotalTokens
		if total == 0 {
			total = u.Input + u.Output + u.CacheRead + u.CacheWrite
		}
		p.sum.Tokens += total
		p.sum.USD += u.Cost.Total
		var texts []string
		for _, c := range m.Content {
			if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
				texts = append(texts, strings.TrimSpace(c.Text))
			}
		}
		text := strings.Join(texts, "\n")
		if text != "" {
			p.text = text
		}
		if m.StopReason == "error" || m.StopReason == "aborted" {
			p.failed = true
			p.errText = m.ErrorMessage
			if p.errText == "" {
				p.errText = "the run ended with stop reason " + m.StopReason
			}
			return "[error] " + firstLine(p.errText, maxProgress)
		}
		return text
	case "tool_execution_start":
		return "[tool " + ev.ToolName + "] " + toolHint(ev.Args)
	case "tool_execution_end":
		if ev.IsError {
			var b strings.Builder
			for _, c := range ev.Result.Content {
				b.WriteString(c.Text)
			}
			return "[tool error] " + firstLine(b.String(), maxProgress)
		}
	case "auto_retry_start":
		return fmt.Sprintf("[retry %d/%d] %s", ev.Attempt, ev.MaxAttempts, firstLine(ev.ErrorMessage, maxProgress))
	}
	return ""
}

func (p *ompParser) Summary() Summary {
	s := p.sum
	s.Text = p.text
	if p.failed {
		s.Failed, s.Err = true, p.errText
	}
	return s
}

var (
	reANSI = regexp.MustCompile("\x1b(?:\\[[0-?]*[ -/]*[@-~]|\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|[@-Z\\\\-_])")
)

// StripANSI removes terminal colour codes.
func StripANSI(s string) string { return reANSI.ReplaceAllString(s, "") }
