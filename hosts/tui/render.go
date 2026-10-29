package tui

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Styles. With no color profile (tests, pipes) they render as plain text.
var (
	styleUser    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	styleDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleBold    = lipgloss.NewStyle().Bold(true)
	styleCode    = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleErr     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleOK      = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleAdd     = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleDel     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleHunk    = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	styleWarn    = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)
	styleStatus  = lipgloss.NewStyle().Reverse(true)
	styleToolHdr = lipgloss.NewStyle().Bold(true)
)

var (
	reBold = regexp.MustCompile(`\*\*([^*]+?)\*\*`)
	reCode = regexp.MustCompile("`([^`]+)`")
	reList = regexp.MustCompile(`^(\s*)[-*+]\s+(.*)$`)
	reHead = regexp.MustCompile(`^#{1,6}\s+(.*)$`)
)

// wrap word-wraps s (which may hold ANSI sequences) to width columns.
func wrap(s string, width int) string {
	if width <= 0 {
		return s
	}
	return ansi.Wrap(s, width, "")
}

func inline(s string) string {
	s = reCode.ReplaceAllStringFunc(s, func(m string) string { return styleCode.Render(m[1 : len(m)-1]) })
	return reBold.ReplaceAllStringFunc(s, func(m string) string { return styleBold.Render(m[2 : len(m)-2]) })
}

// RenderMarkdown renders the subset of markdown assistants use: fenced code
// blocks, headings, bullet lists, **bold** and `code`. An unclosed fence (a
// streaming message) is rendered as code to the end.
func RenderMarkdown(text string, width int) string {
	var out []string
	inFence := false
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		trim := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trim, "```"):
			if !inFence {
				if lang := strings.TrimSpace(strings.TrimPrefix(trim, "```")); lang != "" {
					out = append(out, styleDim.Render("┌ "+lang))
				} else {
					out = append(out, styleDim.Render("┌"))
				}
			} else {
				out = append(out, styleDim.Render("└"))
			}
			inFence = !inFence
		case inFence:
			out = append(out, styleDim.Render("│ ")+styleCode.Render(clip(line, width-2)))
		case reHead.MatchString(line):
			out = append(out, wrap(styleBold.Render(reHead.FindStringSubmatch(line)[1]), width))
		case reList.MatchString(line):
			m := reList.FindStringSubmatch(line)
			body := wrap(inline(m[2]), width-len(m[1])-2)
			for i, l := range strings.Split(body, "\n") {
				if i == 0 {
					out = append(out, m[1]+"• "+l)
				} else {
					out = append(out, m[1]+"  "+l)
				}
			}
		default:
			out = append(out, wrap(inline(line), width))
		}
	}
	return strings.Join(out, "\n")
}

// RenderUser renders a message the user sent.
func RenderUser(text string, width int) string {
	return styleUser.Render("> ") + wrap(text, width-2)
}

// RenderSteer renders a message sent to a running run. delivered=false means
// the run ended first and the message waits for the next run.
func RenderSteer(text string, delivered bool) string {
	s := styleUser.Render("↳ steer: ") + text
	if !delivered {
		s += styleDim.Render("  (not delivered; kept for the next run)")
	}
	return s
}

// RenderReasoning renders the model's reasoning as one dim line.
func RenderReasoning(chars int, open bool) string {
	if open {
		return styleDim.Render("✻ thinking…")
	}
	return styleDim.Render(fmt.Sprintf("✻ thought (%d chars)", chars))
}

// RenderRunError renders RUN_ERROR.
func RenderRunError(code, msg string) string {
	s := "error"
	if code != "" {
		s += " [" + code + "]"
	}
	return styleErr.Render(s+": ") + msg
}

// RenderNotice renders an informational line (compaction, cancel…).
func RenderNotice(text string) string { return styleDim.Render("· " + text) }

// ToolView is the state of one tool call.
type ToolView struct {
	Name   string
	Args   string // JSON arguments
	Result string
	Output string // live output (work.tool_output) while the call runs
	Done   bool
	Err    bool
}

// Line limits of tool blocks.
const (
	collapsedLines = 3
	expandedLines  = 40
)

// RenderToolCall renders a tool call: header (name and key argument), the
// clipped result, and for a successful edit_file the diff of oldText/newText.
func RenderToolCall(t ToolView, width int, expanded bool) string {
	mark := styleDim.Render("…")
	switch {
	case t.Done && t.Err:
		mark = styleErr.Render("✗")
	case t.Done:
		mark = styleOK.Render("✓")
	}
	hdr := mark + " " + styleToolHdr.Render(t.Name)
	if s := ToolSummary(t.Args); s != "" {
		hdr += " " + styleDim.Render(clip(s, max(width-len(t.Name)-6, 10)))
	}
	lines := []string{hdr}
	limit := collapsedLines
	if expanded {
		limit = expandedLines
		if pretty := prettyJSON(t.Args); pretty != "" && pretty != "{}" {
			lines = append(lines, bodyLines(styleDim.Render("args"), strings.Split(pretty, "\n"), width, limit, styleDim)...)
		}
	}
	switch {
	case !t.Done:
		if out := strings.TrimRight(t.Output, "\n"); out != "" {
			ls := strings.Split(out, "\n")
			if len(ls) > collapsedLines {
				ls = ls[len(ls)-collapsedLines:]
			}
			lines = append(lines, bodyLines("", ls, width, collapsedLines, styleDim)...)
		}
	case t.Err:
		lines = append(lines, bodyLines("", strings.Split(strings.TrimSpace(t.Result), "\n"), width, limit, styleErr)...)
	case t.Name == "edit_file" && editDiff(t) != "":
		summary := editSummary(t.Result)
		lines = append(lines, bodyLines("", []string{summary}, width, 1, lipgloss.NewStyle())...)
		d := strings.Split(editDiff(t), "\n")
		lines = append(lines, diffLines(d, width, limit*2)...)
	default:
		lines = append(lines, bodyLines("", strings.Split(strings.TrimSpace(t.Result), "\n"), width, limit, styleDim)...)
	}
	return strings.Join(lines, "\n")
}

// bodyLines indents lines under a header with the "⎿" gutter on the first.
func bodyLines(label string, ls []string, width, limit int, st lipgloss.Style) []string {
	if len(ls) == 1 && strings.TrimSpace(ls[0]) == "" {
		return nil
	}
	var out []string
	more := 0
	for i, l := range ls {
		if i >= limit {
			more = len(ls) - limit
			break
		}
		gutter := "    "
		if i == 0 && label == "" {
			gutter = "  ⎿ "
		} else if i == 0 {
			gutter = "  " + label + " "
		}
		out = append(out, styleDim.Render(gutter)+st.Render(clip(strings.TrimRight(l, " \t\r"), max(width-6, 10))))
	}
	if more > 0 {
		out = append(out, styleDim.Render(fmt.Sprintf("    … +%d lines (ctrl+o expands)", more)))
	}
	return out
}

func diffLines(ls []string, width, limit int) []string {
	var out []string
	for i, l := range ls {
		if i >= limit {
			out = append(out, styleDim.Render(fmt.Sprintf("    … +%d lines", len(ls)-limit)))
			break
		}
		st := lipgloss.NewStyle()
		switch {
		case strings.HasPrefix(l, "@@"):
			st = styleHunk
		case strings.HasPrefix(l, "+"):
			st = styleAdd
		case strings.HasPrefix(l, "-"):
			st = styleDel
		}
		out = append(out, "    "+st.Render(clip(l, max(width-6, 10))))
	}
	return out
}

func prettyJSON(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return ""
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

func editSummary(result string) string {
	var r struct {
		Path         string `json:"path"`
		Replacements int    `json:"replacements"`
	}
	if json.Unmarshal([]byte(result), &r) != nil || r.Path == "" {
		return strings.TrimSpace(result)
	}
	noun := "replacements"
	if r.Replacements == 1 {
		noun = "replacement"
	}
	return fmt.Sprintf("%d %s in %s", r.Replacements, noun, r.Path)
}

// editDiff is the unified-looking diff of an edit_file call, from the
// oldText/newText of its arguments; "" when they are missing.
func editDiff(t ToolView) string {
	var a struct {
		Path    string `json:"path"`
		OldText string `json:"oldText"`
		NewText string `json:"newText"`
	}
	if json.Unmarshal([]byte(t.Args), &a) != nil || a.OldText == "" {
		return ""
	}
	return RenderDiff(a.Path, a.OldText, a.NewText)
}

// RenderDiff is a unified-looking diff of one replacement: a hunk header and
// the old lines as "-" and the new ones as "+" (common leading and trailing
// lines are kept as context).
func RenderDiff(path, oldText, newText string) string {
	o, n := strings.Split(oldText, "\n"), strings.Split(newText, "\n")
	pre := 0
	for pre < len(o) && pre < len(n) && o[pre] == n[pre] {
		pre++
	}
	suf := 0
	for suf < len(o)-pre && suf < len(n)-pre && o[len(o)-1-suf] == n[len(n)-1-suf] {
		suf++
	}
	lines := []string{fmt.Sprintf("@@ %s @@", path)}
	for _, l := range o[:pre] {
		lines = append(lines, " "+l)
	}
	for _, l := range o[pre : len(o)-suf] {
		lines = append(lines, "-"+l)
	}
	for _, l := range n[pre : len(n)-suf] {
		lines = append(lines, "+"+l)
	}
	for _, l := range o[len(o)-suf:] {
		lines = append(lines, " "+l)
	}
	return strings.Join(lines, "\n")
}

// StatusView is the data of the status line.
type StatusView struct {
	Model   string
	State   string // idle, running, cancelling, waiting
	Steps   int
	Usage   Usage
	Elapsed string
	Hint    string
	Mode    string // plan or build; "" when the backend has no modes
	Effort  string // reasoning effort; "" is the model's default
}

// FormatTokens abbreviates a token count (1234 → 1.2k).
func FormatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

// RenderUsage is the token part of the status line.
func RenderUsage(u Usage) string {
	s := fmt.Sprintf("in %s out %s", FormatTokens(u.In), FormatTokens(u.Out))
	if u.Cached > 0 {
		s += fmt.Sprintf(" cached %s (%d%%)", FormatTokens(u.Cached), u.CachePercent())
	}
	return s
}

// RenderStatus renders the status line, padded to width.
func RenderStatus(s StatusView, width int) string {
	parts := []string{s.Model, s.State}
	if s.Effort != "" {
		parts = []string{s.Model, "effort " + s.Effort, s.State}
	}
	if s.Mode != "" {
		parts = append([]string{strings.ToUpper(s.Mode)}, parts...)
	}
	if s.Steps > 0 {
		parts = append(parts, fmt.Sprintf("step %d", s.Steps))
	}
	if s.Usage.In+s.Usage.Out > 0 {
		parts = append(parts, RenderUsage(s.Usage))
	}
	if s.Elapsed != "" {
		parts = append(parts, s.Elapsed)
	}
	line := " " + strings.Join(parts, " · ")
	if s.Hint != "" {
		line += "  " + s.Hint
	}
	if pad := width - lipgloss.Width(line); pad > 0 {
		line += strings.Repeat(" ", pad)
	}
	return styleStatus.Render(line)
}

// Prompt is a pending question to the user (an interrupt of the run).
type Prompt struct {
	Reason   string // tool_approval | ask_user
	Title    string // tool name, or the question
	Detail   string // tool input preview
	Options  []string
	FreeText bool
}

// RenderPrompt renders the approval or question the run is waiting on.
func RenderPrompt(p Prompt, width int) string {
	if p.Reason == "tool_approval" {
		lines := []string{styleWarn.Render("Approval needed: ") + styleBold.Render(p.Title)}
		if p.Detail != "" {
			lines = append(lines, "  "+styleDim.Render(clip(oneLine(p.Detail), width-4)))
		}
		lines = append(lines, "  [y] approve once   [a] always (this run)   [n/esc] deny")
		return strings.Join(lines, "\n")
	}
	lines := []string{styleWarn.Render("Question: ") + wrap(p.Title, width-10)}
	for i, o := range p.Options {
		lines = append(lines, fmt.Sprintf("  [%d] %s", i+1, o))
	}
	switch {
	case p.FreeText && len(p.Options) > 0:
		lines = append(lines, styleDim.Render("  press a number, or type an answer and press enter"))
	case p.FreeText:
		lines = append(lines, styleDim.Render("  type an answer and press enter"))
	default:
		lines = append(lines, styleDim.Render("  press a number"))
	}
	return strings.Join(lines, "\n")
}
