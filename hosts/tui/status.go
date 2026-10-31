package tui

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/7ar-chat/harness/hosts/client"
)

// StatusItem is one line of the read-only dialogs /status and /mcp: a fact
// about the session, grouped under a Section ("Backend", "Config", "MCP"…).
// A backend must not put secrets in it.
type StatusItem struct {
	Section string
	Label   string
	Value   string
}

// Sections that /status and /mcp know by name.
const (
	SectionBackend = "Backend"
	SectionConfig  = "Config"
	SectionMCP     = "MCP"
)

// StatusBackend is implemented by a backend that can describe itself beyond
// what the UI knows: the configuration layers, the MCP servers. A backend
// without it still gets /status, from what the UI has.
type StatusBackend interface {
	Status(ctx context.Context) ([]StatusItem, error)
}

func init() {
	register(
		command{name: "status", bind: "i", help: "show the backend, model, workspace and the configuration in effect", run: (*Model).cmdStatus},
		command{name: "skills", help: "list the installed skills", run: (*Model).cmdSkills},
		command{name: "mcp", help: "list the MCP servers", run: (*Model).cmdMCP},
	)
}

// Status implements StatusBackend: the kind of backend, then whatever
// StatusFunc (set by a host that runs the engine in process) adds.
func (b *ClientBackend) Status(ctx context.Context) ([]StatusItem, error) {
	kind := "in-process"
	if b.RemoteURL != "" {
		kind = "remote " + urlHost(b.RemoteURL)
	}
	items := []StatusItem{{SectionBackend, "kind", kind}}
	if b.StatusFunc == nil {
		return items, nil
	}
	more, err := b.StatusFunc(ctx)
	return append(items, more...), err
}

// urlHost is the host (and port) of a URL, without credentials, path or query.
func urlHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return "(unparsable address)"
}

// statusMsg is the answer of the data a dialog needs from the backend.
type statusMsg struct {
	title string
	items []StatusItem
	notes []string // problems, shown as notices
}

func (s statusMsg) apply(m *Model) tea.Cmd {
	for _, n := range s.notes {
		m.notice("%s", n)
	}
	if len(s.items) > 0 || len(s.notes) == 0 {
		m.openInfo(s.title, s.items)
	}
	return nil
}

// openInfo shows items in a read-only list (enter and esc close it).
func (m *Model) openInfo(title string, items []StatusItem) {
	if len(items) == 0 {
		m.notice("%s: nothing to show", strings.ToLower(title))
		return
	}
	pi := make([]pickItem, len(items))
	width := 0
	for _, it := range items {
		width = max(width, len(it.Label))
	}
	for i, it := range items {
		pi[i] = pickItem{id: it.Label, label: fmt.Sprintf("%-*s", min(width, 22), it.Label), desc: it.Value, group: it.Section}
	}
	m.picker = &picker{title: title, hint: "↑↓ · type to filter · esc closes", items: pi, rows: 14,
		pick: func(*Model, pickItem) tea.Cmd { return nil }}
}

// baseStatus is what the UI itself knows about the session.
func (m *Model) baseStatus() []StatusItem {
	const s = "Session"
	items := []StatusItem{{s, "model", m.be.Model()}}
	if rb, ok := m.runOpts(); ok {
		items = append(items, StatusItem{s, "mode", rb.Mode()}, StatusItem{s, "effort", effortName(rb.Effort())})
	}
	if ta, ok := m.be.(ThreadAdminBackend); ok {
		items = append(items, StatusItem{s, "thread", ta.CurrentThread()})
	}
	ws := m.inp.workspace
	if ws == "" {
		ws = "(none: remote session)"
	}
	return append(items, StatusItem{s, "workspace", tildePath(ws)})
}

func (m *Model) cmdStatus(string) tea.Cmd {
	base := m.baseStatus()
	sb, hasStatus := m.be.(StatusBackend)
	cb, hasCmds := m.be.(CommandBackend)
	ctx := m.ctx
	return func() tea.Msg {
		msg := statusMsg{title: "Status", items: base}
		if hasStatus {
			items, err := sb.Status(ctx)
			if err != nil {
				msg.notes = append(msg.notes, "status: "+unsupportedText(err))
			}
			msg.items = append(msg.items, items...)
		}
		if hasCmds {
			if cs, err := cb.Commands(ctx); err == nil {
				msg.items = append(msg.items, commandItems(cs)...)
			}
		}
		sortSections(msg.items)
		return msg
	}
}

func (m *Model) cmdSkills(string) tea.Cmd {
	cb, ok := m.be.(CommandBackend)
	if !ok {
		m.notice("/skills: not supported by this backend/server")
		return nil
	}
	ctx := m.ctx
	return func() tea.Msg {
		cs, err := cb.Commands(ctx)
		if err != nil {
			return statusMsg{title: "Skills", notes: []string{"skills: " + unsupportedText(err)}}
		}
		var skills []client.Command
		for _, c := range cs {
			if c.Source == "skill" {
				skills = append(skills, c)
			}
		}
		items := make([]StatusItem, len(skills))
		for i, c := range skills {
			items[i] = StatusItem{"Skills", "/" + c.Name, c.Description}
		}
		return statusMsg{title: "Skills", items: items}
	}
}

func (m *Model) cmdMCP(string) tea.Cmd {
	sb, ok := m.be.(StatusBackend)
	if !ok {
		m.notice("/mcp: this backend does not report MCP servers")
		return nil
	}
	ctx := m.ctx
	return func() tea.Msg {
		all, err := sb.Status(ctx)
		msg := statusMsg{title: "MCP servers"}
		if err != nil {
			msg.notes = append(msg.notes, "mcp: "+unsupportedText(err))
		}
		for _, it := range all {
			if it.Section == SectionMCP {
				msg.items = append(msg.items, it)
			}
		}
		if len(msg.items) == 0 && err == nil {
			msg.notes = append(msg.notes, "no MCP servers are configured (or this server does not report them)")
		}
		return msg
	}
}

func unsupportedText(err error) string {
	if errors.Is(err, client.ErrNotSupported) {
		return "not supported by this backend/server"
	}
	return err.Error()
}

// commandItems summarises the custom commands and skills a server offers.
func commandItems(cs []client.Command) []StatusItem {
	n := map[string]int{}
	for _, c := range cs {
		n[c.Source]++
	}
	var items []StatusItem
	if k := n["skill"]; k > 0 {
		items = append(items, StatusItem{"Extensions", "skills", fmt.Sprintf("%d (/skills lists them)", k)})
	}
	if k := n["user"] + n["project"]; k > 0 {
		items = append(items, StatusItem{"Extensions", "custom commands", fmt.Sprintf("%d (%d user, %d project)", k, n["user"], n["project"])})
	}
	return items
}

// sectionRank orders the sections of /status; unknown ones follow by name.
var sectionRank = map[string]int{"Session": 0, SectionBackend: 1, SectionConfig: 2, SectionMCP: 3, "Extensions": 4}

// sortSections groups items by section (stable inside a section).
func sortSections(items []StatusItem) {
	rank := func(s string) int {
		if r, ok := sectionRank[s]; ok {
			return r
		}
		return len(sectionRank)
	}
	sort.SliceStable(items, func(i, j int) bool {
		ri, rj := rank(items[i].Section), rank(items[j].Section)
		if ri != rj {
			return ri < rj
		}
		return ri == len(sectionRank) && items[i].Section < items[j].Section
	})
}
