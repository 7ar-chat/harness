package tui

import (
	tea "github.com/charmbracelet/bubbletea"
)

func init() {
	register(command{name: "palette", bind: "p", help: "search all commands (ctrl+p)", run: func(m *Model, _ string) tea.Cmd { return m.openPalette() }})
}

// openPalette opens the picker over every registered command; enter runs the
// one chosen (a command that takes an argument is put in the input).
func (m *Model) openPalette() tea.Cmd {
	cs := commands()
	items := make([]pickItem, len(cs))
	for i, c := range cs {
		label := "/" + c.name
		if c.args != "" {
			label += " " + c.args
		}
		desc := c.help
		if k := m.keys.keyLabel(c); k != "" {
			desc += "  [" + k + "]"
		}
		items[i] = pickItem{id: c.name, label: label, desc: desc}
	}
	m.picker = &picker{title: "commands", items: items, pick: func(m *Model, it pickItem) tea.Cmd {
		if c, ok := lookup(it.id); ok {
			return m.runCommand(c)
		}
		return nil
	}}
	return nil
}
