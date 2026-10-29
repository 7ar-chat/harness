package tui

import (
	"os"
	"strings"
)

// welcome is the screen of an empty conversation: the name, the model, the
// directory, three hints and a tip.
func (m *Model) welcome() string {
	var b strings.Builder
	b.WriteString(styleUser.Render("harness") + styleDim.Render("  a coding agent in your terminal") + "\n\n")
	b.WriteString(styleDim.Render("  model  ") + m.be.Model() + "\n")
	if dir := m.inp.workspace; dir != "" {
		b.WriteString(styleDim.Render("  cwd    ") + tildePath(dir) + "\n")
	}
	b.WriteString("\n")
	b.WriteString(styleDim.Render("  / ") + "for commands   " + styleDim.Render("@ ") + "for files   " + styleDim.Render("! ") + "for the shell\n")
	if t := m.tipLine(); t != "" {
		b.WriteString("\n" + styleDim.Render("  "+t))
	}
	return b.String()
}

// tildePath shortens the home directory prefix of p.
func tildePath(p string) string {
	if h, err := os.UserHomeDir(); err == nil && h != "" && strings.HasPrefix(p, h) {
		return "~" + strings.TrimPrefix(p, h)
	}
	return p
}
