package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/GhaziBenDahmane/harness/engine/agui"
)

// SidebarInfo is what a backend can tell the sidebar beyond the events of the
// run: the MCP servers and skills of the session.
type SidebarInfo struct {
	MCP    []string
	Skills []string
}

// SidebarBackend is implemented by a backend that knows its MCP servers and
// skills. It must not block: the sidebar asks on every render. A backend
// without it shows no such sections.
type SidebarBackend interface {
	SidebarInfo() SidebarInfo
}

// Sidebar layout: shown on its own above sidebarAutoCols, on request above
// sidebarMinCols.
const (
	sidebarWidth    = 34
	sidebarMinCols  = 100
	sidebarAutoCols = 140
)

type sidebarMode int

const (
	sbAuto sidebarMode = iota // shown when the terminal is wide
	sbOn
	sbOff
)

// planEntry is one item of the agent's update_plan list.
type planEntry struct{ ID, Title, Status string }

// sidebar is the state of the right-hand panel.
type sidebar struct {
	mode           sidebarMode
	title          string
	plan           []planEntry
	files          []string // files edited or written, in first-touch order
	cost, costBase float64
	ctx, window    int
}

func init() {
	register(command{name: "sidebar", bind: "b", help: "show / hide the side panel (ctrl+b)", run: func(m *Model, _ string) tea.Cmd { m.toggleSidebar(); return nil }})
}

// sidebarCols is the width the panel takes, 0 when it is hidden.
func (m *Model) sidebarCols() int {
	switch {
	case m.inp.sb.mode == sbOff, m.width < sidebarMinCols:
		return 0
	case m.inp.sb.mode == sbAuto && m.width < sidebarAutoCols:
		return 0
	}
	return sidebarWidth
}

// contentWidth is the width of the transcript.
func (m *Model) contentWidth() int {
	w := max(m.width, 20)
	if c := m.sidebarCols(); c > 0 {
		w -= c + 1
	}
	return w
}

func (m *Model) toggleSidebar() {
	sb := &m.inp.sb
	if m.sidebarCols() > 0 {
		sb.mode = sbOff
		return
	}
	if m.width < sidebarMinCols {
		m.add(block{kind: blockNotice, text: fmt.Sprintf("the side panel needs %d columns", sidebarMinCols)})
		return
	}
	sb.mode = sbOn
}

// withSidebar puts the panel to the right of the transcript view.
func (m *Model) withSidebar(view string) string {
	c := m.sidebarCols()
	if c == 0 {
		return view
	}
	panel := m.renderSidebar(c, lipgloss.Height(view))
	return lipgloss.JoinHorizontal(lipgloss.Top, view, " ", panel)
}

// observe folds the events the sidebar and the notifications care about.
func (m *Model) observe(ev Ev) {
	sb := &m.inp.sb
	switch ev.Type {
	case agui.StateSnapshot:
		var w struct {
			Snapshot struct {
				Plan []planEntry `json:"plan"`
			} `json:"snapshot"`
		}
		if json.Unmarshal(ev.Raw, &w) == nil && w.Snapshot.Plan != nil {
			sb.plan = w.Snapshot.Plan
		}
	case agui.Custom:
		switch ev.Name {
		case agui.CustomTitle:
			if t, _ := ev.Value["title"].(string); t != "" {
				sb.title = t
			}
		case agui.CustomUsage:
			sb.ctx, sb.window = m.cur.Context, m.cur.Window
			if c, ok := ev.Value["cost"].(float64); ok {
				sb.cost = c
			}
		}
	case agui.ToolCallResult:
		i, ok := m.toolIdx[ev.ToolCallID]
		if !ok || ev.IsError {
			break
		}
		t := m.blocks[i].tool
		switch t.Name {
		case "edit_file", "write_file":
			if p := argString(t.Args, "path"); p != "" {
				sb.touch(p)
			}
		}
	case agui.RunFinished:
		sb.costBase += sb.cost
		sb.cost = 0
	}
	m.attention(ev)
}

func (s *sidebar) touch(p string) {
	for _, f := range s.files {
		if f == p {
			return
		}
	}
	s.files = append(s.files, p)
}

// sessionTitle is the title of the conversation: the generated one, else the
// start of the first message.
func (m *Model) sessionTitle() string {
	if t := m.inp.sb.title; t != "" {
		return t
	}
	for _, b := range m.blocks {
		if b.kind == blockUser {
			return clip(oneLine(b.text), 60)
		}
	}
	return ""
}

var planIcon = map[string]string{"pending": "☐", "in_progress": "◐", "completed": "☑"}

// renderPlan lists the plan items with their status icons, one per line.
func renderPlan(plan []planEntry, w int) []string {
	var out []string
	for _, it := range plan {
		icon := planIcon[it.Status]
		if icon == "" {
			icon = "☐"
		}
		line := clip(icon+" "+it.Title, w)
		if it.Status == "completed" {
			line = styleDim.Render(line)
		}
		out = append(out, line)
	}
	return out
}

// renderSidebar draws the panel w columns wide and h rows high.
func (m *Model) renderSidebar(w, h int) string {
	in := w - 2 // border and padding
	sb := &m.inp.sb
	var ls []string
	section := func(name string) { ls = append(ls, "", styleBold.Render(name)) }
	ls = append(ls, styleBold.Render(clip(orDefault(m.sessionTitle(), "New session"), in)))
	if ta, ok := m.be.(ThreadAdminBackend); ok {
		if id := ta.CurrentThread(); id != "" {
			ls = append(ls, styleDim.Render(clip(id, in)))
		}
	}
	section("Model")
	mdl := m.be.Model()
	if rb, ok := m.be.(RunOptionsBackend); ok {
		mdl += " · " + rb.Mode()
	}
	ls = append(ls, clip(mdl, in))
	section("Context")
	u := m.base
	u.add(m.cur)
	ls = append(ls, styleDim.Render(fmt.Sprintf("in %s out %s", FormatTokens(u.In), FormatTokens(u.Out))))
	if sb.ctx > 0 {
		c := fmt.Sprintf("window %s", FormatTokens(sb.ctx))
		if sb.window > 0 {
			c = fmt.Sprintf("window %s / %s (%d%%)", FormatTokens(sb.ctx), FormatTokens(sb.window), sb.ctx*100/sb.window)
		}
		ls = append(ls, styleDim.Render(clip(c, in)))
	}
	if cost := sb.costBase + sb.cost; cost > 0 {
		ls = append(ls, styleDim.Render(fmt.Sprintf("cost $%.4f", cost)))
	}
	if len(sb.plan) > 0 {
		section("Plan")
		ls = append(ls, renderPlan(sb.plan, in)...)
	}
	if len(sb.files) > 0 {
		section("Modified files")
		for _, f := range sb.files {
			ls = append(ls, clip(f, in))
		}
	}
	if si, ok := m.be.(SidebarBackend); ok {
		info := si.SidebarInfo()
		if len(info.MCP) > 0 {
			section("MCP")
			for _, s := range info.MCP {
				ls = append(ls, clip(s, in))
			}
		}
		if len(info.Skills) > 0 {
			section("Skills")
			for _, s := range info.Skills {
				ls = append(ls, clip(s, in))
			}
		}
	}
	if len(ls) > h {
		ls = append(ls[:h-1], styleDim.Render("…"))
	}
	bar := styleDim.Render("│ ")
	out := make([]string, h)
	for i := range out {
		line := ""
		if i < len(ls) {
			line = ls[i]
		}
		out[i] = bar + lipgloss.NewStyle().Width(in).Render(line)
	}
	return strings.Join(out, "\n")
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
