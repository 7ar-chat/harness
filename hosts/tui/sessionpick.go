package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/7ar-chat/harness/hosts/client"
)

// The conversation picker: switch (enter), rename (ctrl+r), delete (ctrl+d,
// asks to confirm) and pin (ctrl+o), the last three through ThreadAdminBackend.

// sessionsAdminMsg: a rename, delete or pin finished; the picker opens again on the fresh list.
type sessionsAdminMsg struct {
	op     string
	notice string
	list   []client.Session
	err    error
}

func (s sessionsAdminMsg) apply(m *Model) tea.Cmd {
	switch {
	case s.err != nil:
		if !m.unsupported(s.op, s.err) {
			m.notice("%s: %v", s.op, s.err)
		}
	default:
		if s.notice != "" {
			m.notice("%s", s.notice)
		}
		m.openSessionPickerX(s.list)
	}
	return nil
}

func sessionTitle(s client.Session) string {
	if s.Title == "" {
		return "(untitled)"
	}
	return s.Title
}

func (m *Model) openSessionPickerX(list []client.Session) {
	if len(list) == 0 {
		m.notice("no saved conversations")
		return
	}
	// Pinned first, each group keeping the order of the backend (newest first).
	list = append([]client.Session(nil), list...)
	sort.SliceStable(list, func(i, j int) bool { return list[i].Pinned && !list[j].Pinned })
	pinned := map[string]bool{}
	items := make([]pickItem, 0, len(list))
	for _, s := range list {
		label := sessionTitle(s)
		if s.Pinned {
			pinned[s.ID] = true
			label = "★ " + label
		}
		items = append(items, pickItem{id: s.ID, label: label,
			desc: fmt.Sprintf("%s · %d msgs · %s", s.UpdatedAt.Format("Jan 2 15:04"), s.Messages, clip(oneLine(s.Preview), 50))})
	}
	p := &picker{title: "Conversations", items: items, pick: func(m *Model, it pickItem) tea.Cmd {
		sb := m.be.(SessionBackend)
		return func() tea.Msg {
			th, err := sb.OpenThread(m.ctx, it.id)
			return threadMsg{th, err}
		}
	}}
	m.picker = p
	ta, ok := m.be.(ThreadAdminBackend)
	if !ok {
		return
	}
	p.hint = "↑↓ · enter · ctrl+r rename · ctrl+d delete · ctrl+o pin · esc"
	var mode, text, id string // mode: "" browsing, "rename" typing a title, "delete" waiting for y
	titles := map[string]string{}
	for _, s := range list {
		titles[s.ID] = sessionTitle(s)
	}
	// admin runs op on the backend, then lists again so the picker shows the result.
	admin := func(m *Model, op, notice string, do func() error) tea.Cmd {
		sb, _ := m.be.(SessionBackend)
		return func() tea.Msg {
			if err := do(); err != nil {
				return sessionsAdminMsg{op: op, err: err}
			}
			if sb == nil {
				return sessionsAdminMsg{op: op, notice: notice}
			}
			l, err := sb.Sessions(m.ctx)
			return sessionsAdminMsg{op: op, notice: notice, list: l, err: err}
		}
	}
	p.extra = func(m *Model, k tea.KeyMsg) (tea.Cmd, bool) {
		s := k.String()
		switch mode {
		case "rename":
			switch {
			case s == "esc":
				mode, p.footer = "", ""
			case s == "enter":
				title := strings.TrimSpace(text)
				rid := id // the command runs later
				mode, p.footer = "", ""
				if title == "" {
					return nil, true
				}
				return admin(m, "rename", "renamed: "+title, func() error { return ta.RenameSession(m.ctx, rid, title) }), true
			case s == "backspace":
				if r := []rune(text); len(r) > 0 {
					text = string(r[:len(r)-1])
				}
			case k.Type == tea.KeyRunes || k.Type == tea.KeySpace:
				text += string(k.Runes) // a KeySpace already carries its space in Runes
			}
			if mode == "rename" {
				p.footer = styleBold.Render("rename: ") + text + "▏" + styleDim.Render("  enter saves · esc cancels")
			}
			return nil, true
		case "delete":
			rid := id
			mode, p.footer = "", ""
			if s == "y" || s == "Y" {
				return admin(m, "delete", "deleted: "+titles[rid], func() error { return ta.DeleteSession(m.ctx, rid) }), true
			}
			return nil, true
		}
		vis := p.visible()
		if (s != "ctrl+r" && s != "ctrl+d" && s != "ctrl+o") || p.sel >= len(vis) {
			return nil, false
		}
		id = vis[p.sel].id
		id := id // the commands below run later
		switch s {
		case "ctrl+r":
			mode, text = "rename", titles[id]
			if text == "(untitled)" {
				text = ""
			}
			p.footer = styleBold.Render("rename: ") + text + "▏" + styleDim.Render("  enter saves · esc cancels")
		case "ctrl+d":
			mode = "delete"
			p.footer = styleWarn.Render(fmt.Sprintf("delete %q? ", clip(titles[id], 40))) + styleDim.Render("y deletes · any other key cancels")
			if id == ta.CurrentThread() {
				p.footer += styleWarn.Render("  (this is the current conversation)")
			}
		case "ctrl+o":
			pin := !pinned[id]
			verb := "pinned"
			if !pin {
				verb = "unpinned"
			}
			return admin(m, "pin", verb+": "+titles[id], func() error { return ta.PinSession(m.ctx, id, pin) }), true
		}
		return nil, true
	}
}
