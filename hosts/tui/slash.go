package tui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"

	tea "github.com/charmbracelet/bubbletea"
)

// command is a slash command typed in the input box.
type command struct {
	name    string
	aliases []string
	args    string // usage of the argument, "" when there is none
	help    string
	// bind is the default leader keybind ("n" is leader+n); "" for none. The keymap may override it.
	bind string
	run  func(m *Model, arg string) tea.Cmd
}

// reSlash matches "/name" and "/name rest": a path such as /etc/hosts is not a command.
var reSlash = regexp.MustCompile(`^/([a-z][a-z-]*)(?:\s+([\s\S]*))?$`)

// registry holds the slash commands in registration order (Go runs the init
// functions of a package's files in file-name order). Every file that adds
// commands calls register from its own init.
var registry []command

func register(cs ...command) { registry = append(registry, cs...) }

// commands returns the registered slash commands, in the order /help and the completion list show them.
func commands() []command { return withUserCommands(registry) }

func init() {
	register(
		command{name: "help", aliases: []string{"?"}, help: "list the commands and keys", run: (*Model).cmdHelp},
		command{name: "tools", help: "expand / collapse tool calls (ctrl+o)", run: func(m *Model, _ string) tea.Cmd { m.expanded = !m.expanded; return nil }},
		command{name: "quit", aliases: []string{"exit", "q"}, bind: "q", help: "leave", run: func(m *Model, _ string) tea.Cmd { m.quit = true; return tea.Quit }},
	)
}

func (c command) matches(name string) bool {
	if c.name == name {
		return true
	}
	for _, a := range c.aliases {
		if a == name {
			return true
		}
	}
	return false
}

// lookup finds a command by name or alias, or by a unique prefix.
func lookup(name string) (command, bool) {
	var pre []command
	for _, c := range commands() {
		if c.matches(name) {
			return c, true
		}
		if strings.HasPrefix(c.name, name) {
			pre = append(pre, c)
		}
	}
	if len(pre) == 1 {
		return pre[0], true
	}
	return command{}, false
}

// suggest lists the commands the partial "/na" can complete to.
func suggest(input string) []command {
	if !strings.HasPrefix(input, "/") || strings.ContainsAny(input, " \n") {
		return nil
	}
	p := input[1:]
	var pre, sub []command
	for _, c := range commands() {
		switch {
		case strings.HasPrefix(c.name, p):
			pre = append(pre, c)
		case strings.Contains(c.name, p):
			sub = append(sub, c)
		}
	}
	return append(pre, sub...)
}

// slash runs the command in text. handled=false means text is not a command
// (it is sent to the model). A leading space sends a "/…" message literally.
func (m *Model) slash(raw string) (cmd tea.Cmd, handled bool) {
	text := strings.TrimSpace(raw)
	sm := reSlash.FindStringSubmatch(text)
	if !strings.HasPrefix(raw, "/") || sm == nil {
		return nil, false
	}
	c, ok := lookup(sm[1])
	if !ok {
		m.add(block{kind: blockNotice, text: fmt.Sprintf("unknown command /%s — /help lists them (start the message with a space to send it as is)", sm[1])})
		return nil, true
	}
	m.in.Reset()
	return c.run(m, strings.TrimSpace(sm[2])), true
}

func (m *Model) cmdHelp(string) tea.Cmd {
	var b strings.Builder
	b.WriteString("commands\n")
	for _, c := range commands() {
		use := "/" + c.name
		if c.args != "" {
			use += " " + c.args
		}
		fmt.Fprintf(&b, "  %-16s %s\n", use, c.help)
	}
	b.WriteString(helpExtra())
	b.WriteString("keys\n")
	b.WriteString("  enter sends (steers while a run is active) · alt+enter new line · esc cancels the run\n")
	b.WriteString("  ctrl+o tools · pgup/pgdn scroll · ctrl+c twice quits")
	m.add(block{kind: blockNotice, text: b.String()})
	return nil
}

// renderSuggestions draws the completion list above the input.
func renderSuggestions(cs []command, sel, width int) string {
	lines := make([]string, 0, len(cs))
	for i, c := range cs {
		name := "/" + c.name
		if c.args != "" {
			name += " " + c.args
		}
		line := fmt.Sprintf("  %-14s %s", name, c.help)
		line = clip(line, width)
		if i == sel {
			line = styleSel.Render(line)
		} else {
			line = styleDim.Render(line)
		}
		lines = append(lines, line)
	}
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}
