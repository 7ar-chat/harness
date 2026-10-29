package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/GhaziBenDahmane/harness/engine/agui"
)

// Limits of the sub-agent display.
const (
	subTailLines = 5   // tool lines shown while the sub-agent runs (all when expanded)
	subMaxLines  = 500 // lines kept per sub-agent
)

func init() {
	register(command{name: "children", aliases: []string{"subagents"}, bind: "down", help: "browse the sub-agent runs of this conversation (leader + down)", run: (*Model).cmdChildren})
}

// SubEvent is a decoded work.subagent CUSTOM event.
type SubEvent struct {
	ToolCallID  string
	Phase       string // start, step, tool, text, end
	Agent       string
	Task        string
	Model       string
	Step        int
	Text        string
	Tool        SubTool
	Usage       Usage // cumulative tokens of the sub-agent
	ReportChars int
	Failed      bool
}

// SubTool is one tool call of a sub-agent.
type SubTool struct {
	Name, Args, Result string
	Err                bool
}

// SubagentValue reads a work.subagent CUSTOM value.
func (e Ev) SubagentValue() SubEvent {
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	num := func(m map[string]any, k string) int { f, _ := m[k].(float64); return int(f) }
	s := SubEvent{ToolCallID: str(e.Value, "toolCallId"), Phase: str(e.Value, "phase"), Agent: str(e.Value, "agent"),
		Task: str(e.Value, "task"), Model: str(e.Value, "model"), Step: num(e.Value, "step"), Text: str(e.Value, "text"),
		ReportChars: num(e.Value, "reportChars")}
	s.Failed, _ = e.Value["failed"].(bool)
	if t, ok := e.Value["tool"].(map[string]any); ok {
		s.Tool = SubTool{Name: str(t, "name"), Args: str(t, "args"), Result: str(t, "result")}
		s.Tool.Err, _ = t["isError"].(bool)
	}
	if u, ok := e.Value["usage"].(map[string]any); ok {
		s.Usage = Usage{In: num(u, "in"), Out: num(u, "out"), Cached: num(u, "cached")}
	}
	return s
}

// SubLine is one line of a sub-agent's activity: a tool call, or a remark
// of the model between two steps (Tool.Name empty).
type SubLine struct {
	Tool SubTool
	Text string
}

// Subagent is the state of one sub-agent run, built from its events.
type Subagent struct {
	Agent, Task, Model string
	Step               int
	Usage              Usage
	Lines              []SubLine
	Dropped            int // oldest lines dropped past subMaxLines
	Done, Failed       bool
	ReportChars        int
}

// Apply folds one event into the run.
func (s *Subagent) Apply(e SubEvent) {
	if e.Agent != "" {
		s.Agent = e.Agent
	}
	if e.Model != "" {
		s.Model = e.Model
	}
	if e.Step > s.Step {
		s.Step = e.Step
	}
	if e.Usage.In+e.Usage.Out > 0 {
		s.Usage = e.Usage
	}
	switch e.Phase {
	case agui.SubagentStart:
		s.Task = e.Task
	case agui.SubagentTool:
		s.addLine(SubLine{Tool: e.Tool})
	case agui.SubagentText:
		s.addLine(SubLine{Text: e.Text})
	case agui.SubagentEnd:
		s.Done, s.Failed, s.ReportChars = true, e.Failed, e.ReportChars
	}
}

func (s *Subagent) addLine(l SubLine) {
	if len(s.Lines) >= subMaxLines {
		s.Lines = s.Lines[1:]
		s.Dropped++
	}
	s.Lines = append(s.Lines, l)
}

// subagentEvent folds a work.subagent event into the tool block it belongs to.
func (m *Model) subagentEvent(ev Ev) {
	e := ev.SubagentValue()
	i, ok := m.toolIdx[e.ToolCallID]
	if !ok {
		return
	}
	t := &m.blocks[i].tool
	if t.Sub == nil {
		t.Sub = &Subagent{Task: argString(t.Args, "task")}
	}
	t.Sub.Apply(e)
}

// subHeader is the first line of a sub-agent block: `⎇ explore  "task"  step 3 · 1.2k tokens`.
func subHeader(mark string, t ToolView, width int) string {
	s := t.Sub
	agent := s.Agent
	if agent == "" {
		agent = t.Name
	}
	task := s.Task
	if task == "" {
		task = argString(t.Args, "task")
	}
	hdr := mark + " " + styleToolHdr.Render("⎇ "+agent)
	if task = oneLine(task); task != "" {
		hdr += "  " + styleDim.Render(strconv.Quote(clip(task, max(width-40, 20))))
	}
	var meta []string
	if s.Step > 0 {
		meta = append(meta, fmt.Sprintf("step %d", s.Step))
	}
	if n := s.Usage.In + s.Usage.Out; n > 0 {
		meta = append(meta, FormatTokens(n)+" tokens")
	}
	if len(meta) > 0 {
		hdr += "  " + styleDim.Render(strings.Join(meta, " · "))
	}
	return hdr
}

// subLine renders one activity line; withResult adds the short result of a tool call.
func subLine(l SubLine, width int, withResult bool) string {
	if l.Tool.Name == "" {
		return styleDim.Render("    · " + clip(oneLine(l.Text), max(width-8, 10)))
	}
	mark := styleOK.Render("✓")
	if l.Tool.Err {
		mark = styleErr.Render("✗")
	}
	s := "    " + mark + " " + styleToolHdr.Render(l.Tool.Name)
	if l.Tool.Args != "" {
		s += " " + styleDim.Render(clip(l.Tool.Args, max(width-len(l.Tool.Name)-10, 10)))
	}
	if withResult && l.Tool.Result != "" {
		s += styleDim.Render("  → " + clip(l.Tool.Result, 60))
	}
	return s
}

// renderSubagent renders the explore block with its live nested view: the
// header, the sub-agent's last tool calls (all when expanded) and the report.
func renderSubagent(t ToolView, width int, expanded bool) string {
	mark := styleDim.Render("…")
	switch {
	case t.Done && t.Err:
		mark = styleErr.Render("✗")
	case t.Done:
		mark = styleOK.Render("✓")
	}
	s := t.Sub
	lines := []string{subHeader(mark, t, width)}
	body := s.Lines
	if !expanded {
		// text remarks only show expanded
		body = nil
		for _, l := range s.Lines {
			if l.Tool.Name != "" {
				body = append(body, l)
			}
		}
		if len(body) > subTailLines {
			lines = append(lines, styleDim.Render(fmt.Sprintf("    … %d earlier", len(body)-subTailLines)))
			body = body[len(body)-subTailLines:]
		}
	}
	for _, l := range body {
		lines = append(lines, subLine(l, width, expanded))
	}
	if t.Done {
		limit, st := collapsedLines, styleDim
		if expanded {
			limit = 1000
		}
		if t.Err {
			st = styleErr
		}
		lines = append(lines, bodyLines("", strings.Split(strings.TrimSpace(t.Result), "\n"), width, limit, st)...)
	}
	return strings.Join(lines, "\n")
}

// ---- /children ----

// subState is the sub-agent browser: the block shown in the read-only pane.
type subState struct {
	open  bool
	block int // index into Model.blocks
	off   int // first visible line
}

// childBlocks are the indexes of the blocks that ran a sub-agent.
func (m *Model) childBlocks() []int {
	var out []int
	for i, b := range m.blocks {
		if b.kind == blockTool && (b.tool.Sub != nil || b.tool.Name == "explore") {
			out = append(out, i)
		}
	}
	return out
}

func (m *Model) cmdChildren(string) tea.Cmd {
	idx := m.childBlocks()
	if len(idx) == 0 {
		m.notice("no sub-agent ran in this conversation")
		return nil
	}
	items := make([]pickItem, 0, len(idx))
	for n, i := range idx {
		t := m.blocks[i].tool
		task := argString(t.Args, "task")
		state := "running"
		switch {
		case t.Sub == nil:
			state = "report only"
		case t.Done && t.Err:
			state = "failed"
		case t.Done:
			state = "done"
		}
		if t.Sub != nil {
			if t.Sub.Task != "" {
				task = t.Sub.Task
			}
			state += fmt.Sprintf(" · %d tools", len(t.Sub.Lines)+t.Sub.Dropped)
		}
		items = append(items, pickItem{id: strconv.Itoa(i), label: fmt.Sprintf("%d. explore  %q", n+1, clip(oneLine(task), 60)), desc: state})
	}
	m.picker = &picker{title: "Sub-agents", items: items, hint: "enter shows the transcript · esc",
		pick: func(m *Model, it pickItem) tea.Cmd {
			i, _ := strconv.Atoi(it.id)
			m.sub = subState{open: true, block: i}
			return nil
		}}
	return nil
}

// subKey scrolls and closes the pane; it consumes every key while it is open.
func (m *Model) subKey(k tea.KeyMsg) (tea.Cmd, bool) {
	if !m.sub.open {
		return nil, false
	}
	switch k.String() {
	case "esc", "q", "ctrl+c":
		m.sub = subState{}
	case "up", "k":
		m.sub.off = max(m.sub.off-1, 0)
	case "down", "j":
		m.sub.off++
	case "pgup":
		m.sub.off = max(m.sub.off-m.height/2, 0)
	case "pgdown", " ":
		m.sub.off += m.height / 2
	case "home":
		m.sub.off = 0
	case "end":
		m.sub.off = 1 << 30
	}
	return nil, true
}

// subTranscript is the full read-only transcript of a sub-agent block.
func subTranscript(t ToolView, width int) []string {
	if t.Sub == nil {
		return []string{
			styleToolHdr.Render("⎇ explore") + "  " + styleDim.Render(strconv.Quote(clip(oneLine(argString(t.Args, "task")), max(width-20, 20)))),
			styleDim.Render("Only the report was saved with the conversation; the sub-agent's steps are not stored."),
			"",
			clip(strings.TrimSpace(t.Result), width*20),
		}
	}
	mark := styleDim.Render("…")
	switch {
	case t.Done && t.Err:
		mark = styleErr.Render("✗")
	case t.Done:
		mark = styleOK.Render("✓")
	}
	lines := []string{subHeader(mark, t, width)}
	if t.Sub.Model != "" {
		lines = append(lines, styleDim.Render("  model "+t.Sub.Model))
	}
	if t.Sub.Dropped > 0 {
		lines = append(lines, styleDim.Render(fmt.Sprintf("    … %d earlier lines not kept", t.Sub.Dropped)))
	}
	for _, l := range t.Sub.Lines {
		lines = append(lines, subLine(l, width, true))
	}
	if t.Done {
		lines = append(lines, "", styleBold.Render("Report"))
		for _, l := range strings.Split(strings.TrimSpace(t.Result), "\n") {
			lines = append(lines, clip(l, width))
		}
	} else {
		lines = append(lines, "", styleDim.Render("running…"))
	}
	return lines
}

// subView is the read-only pane that replaces the screen while it is open.
func (m *Model) subView() string {
	w := max(m.width, 20)
	if m.sub.block >= len(m.blocks) {
		m.sub = subState{}
		return m.View()
	}
	all := subTranscript(m.blocks[m.sub.block].tool, w)
	rows := max(m.height-3, 3)
	if m.sub.off > len(all)-rows {
		m.sub.off = max(len(all)-rows, 0)
	}
	end := min(m.sub.off+rows, len(all))
	out := []string{styleBold.Render("Sub-agent transcript") + styleDim.Render("  ↑↓ pgup/pgdn scroll · esc returns"), ""}
	for _, l := range all[m.sub.off:end] {
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}
