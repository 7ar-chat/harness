package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The styles live in theme.go (applyTheme assigns them); with no color profile
// (tests, pipes) they render as plain text.

// wrap word-wraps s (which may hold ANSI sequences) to width columns.
func wrap(s string, width int) string {
	if width <= 0 {
		return s
	}
	return ansi.Wrap(s, width, "")
}

// RenderUser renders a message the user sent.
func RenderUser(text string, width int) string {
	body := styleUser.Render("> ") + wrap(text, width-2)
	if diffTint.panel == "" {
		return body
	}
	// a tinted panel the width of the transcript
	bg := lipgloss.NewStyle().Background(lipgloss.Color(diffTint.panel))
	ls := strings.Split(body, "\n")
	for i, l := range ls {
		ls[i] = padCellStyled(l, width, bg)
	}
	return strings.Join(ls, "\n")
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
	Output string    // live output (work.tool_output) while the call runs
	Sub    *Subagent // the sub-agent's activity (work.subagent), nil for other tools
	Done   bool
	Err    bool
}

// Line limits of tool blocks.
const (
	collapsedLines = 3
	expandedLines  = 40
	// diffLimit is how many lines of an edit's diff show before "… +N lines" (ctrl+o triples it):
	// the diff is the point of an edit, so it gets far more room than other results.
	diffLimit = 30
)

// RenderToolCall renders a tool call: header (name and key argument), the
// clipped result, and for a successful edit_file the diff of oldText/newText.
func RenderToolCall(t ToolView, width int, expanded bool) string {
	return renderToolCall(t, width, expanded, false)
}

// renderToolCall is RenderToolCall with the choice of the split (side by side) diff.
func renderToolCall(t ToolView, width int, expanded, split bool) string {
	if t.Sub != nil {
		return renderSubagent(t, width, expanded)
	}
	mark := styleDim.Render("…")
	switch {
	case t.Done && t.Err:
		mark = styleErr.Render("✗")
	case t.Done:
		mark = styleOK.Render("✓")
	}
	hdr := mark + " " + styleToolHdr.Render(t.Name)
	if s := ToolSummary(t.Args); s != "" {
		if isShellTool(t.Name) {
			hdr = mark + " " + styleToolHdr.Render("$") + " " + clip(s, max(width-4, 10))
		} else {
			hdr += " " + styleDim.Render(clip(s, max(width-len(t.Name)-6, 10)))
		}
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
		diff := editDiff(t)
		add, del := diffStats(diff)
		lines[0] += "  " + styleAdd.Render(fmt.Sprintf("+%d", add)) + " " + styleDel.Render(fmt.Sprintf("−%d", del))
		if n := editReplacements(t.Result); n > 1 {
			lines[0] += styleDim.Render(fmt.Sprintf(" · %d replacements", n))
		}
		dl := diffLimit
		if expanded {
			dl = diffLimit * 3
		}
		var body []string
		if split {
			body = renderSplitDiff(diff, width, dl)
		}
		if body == nil {
			body = stackedDiff(strings.Split(diff, "\n"), width, dl)
		}
		if len(body) > 0 { // the header already names the file: drop the hunk line
			body = body[1:]
		}
		lines = append(lines, body...)
	case !expanded && collapsedSummary(t) != "":
		lines = append(lines, bodyLines("", []string{collapsedSummary(t)}, width, 1, styleDim)...)
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

// editReplacements is the number of replacements an edit_file result reports (0: unknown).
func editReplacements(result string) int {
	var r struct {
		Replacements int `json:"replacements"`
	}
	if json.Unmarshal([]byte(result), &r) != nil {
		return 0
	}
	return r.Replacements
}

// diffStats counts the added and removed lines of a unified diff.
func diffStats(diff string) (add, del int) {
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "+"):
			add++
		case strings.HasPrefix(l, "-"):
			del++
		}
	}
	return add, del
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
	path := a.Path
	var res struct {
		StartLine int `json:"startLine"`
	}
	if json.Unmarshal([]byte(t.Result), &res) == nil && res.StartLine > 0 {
		path = fmt.Sprintf("%s:%d", a.Path, res.StartLine) // the header carries the first line, see diffTarget
	}
	return RenderDiff(path, a.OldText, a.NewText)
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

	CtxUsed, CtxWindow int    // context tokens of the last step and the model's window (0: unknown)
	Cost               string // session cost, "~" prefixed when partial; "" when unknown
	Spinner            string // frame of the busy spinner; "" when idle
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

// formatWindow abbreviates a context window without a trailing ".0" (200000 → 200k).
func formatWindow(n int) string { return strings.Replace(FormatTokens(n), ".0", "", 1) }

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
	state := s.State
	if s.Spinner != "" {
		state = s.Spinner + " " + state
	}
	parts := []string{s.Model, state}
	if s.Effort != "" {
		parts = []string{s.Model, "effort " + s.Effort, state}
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
	ctx, ctxStyle := renderCtx(s)
	ctxAt := -1
	if ctx != "" {
		ctxAt = len(parts)
		parts = append(parts, ctx)
	}
	if s.Cost != "" {
		parts = append(parts, s.Cost)
	}
	if s.Elapsed != "" {
		parts = append(parts, s.Elapsed)
	}
	// The context part is drawn in its own colour when the window is filling up;
	// the rest of the line keeps the status style.
	head, tail := " "+strings.Join(parts, " · "), ""
	if ctxStyle != nil {
		head = " " + strings.Join(parts[:ctxAt], " · ")
		if ctxAt > 0 {
			head += " · "
		}
		tail = strings.Join(parts[ctxAt+1:], " · ")
		if tail != "" {
			tail = " · " + tail
		}
	}
	rest := tail
	if s.Hint != "" {
		rest += "  " + s.Hint
	}
	w := lipgloss.Width(head) + lipgloss.Width(rest)
	if ctxStyle != nil {
		w += lipgloss.Width(ctx)
	}
	if pad := width - w; pad > 0 {
		rest += strings.Repeat(" ", pad)
	}
	if ctxStyle == nil {
		return styleStatus.Render(head + rest)
	}
	return styleStatus.Render(head) + ctxStyle.Render(ctx) + styleStatus.Render(rest)
}

// renderCtx is "ctx 12.3k/200k (6%)", with the style that flags a window more
// than 70% (warn) or 90% (error) full; nil when it is not.
func renderCtx(s StatusView) (string, *lipgloss.Style) {
	if s.CtxUsed <= 0 {
		return "", nil
	}
	if s.CtxWindow <= 0 {
		return "ctx " + FormatTokens(s.CtxUsed), nil
	}
	pct := s.CtxUsed * 100 / s.CtxWindow
	text := fmt.Sprintf("ctx %s/%s (%d%%)", FormatTokens(s.CtxUsed), formatWindow(s.CtxWindow), pct)
	switch {
	case pct > 90:
		st := styleStatus.Foreground(styleErr.GetForeground())
		return text, &st
	case pct > 70:
		st := styleStatus.Foreground(styleWarn.GetForeground())
		return text, &st
	}
	return text, nil
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
		switch {
		case strings.HasPrefix(p.Title, "mcp_approve__"): // approving a program to run: show all of it
			lines = append(lines, styleDim.Render(wrap(p.Detail, width-4)))
		case p.Detail != "":
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
