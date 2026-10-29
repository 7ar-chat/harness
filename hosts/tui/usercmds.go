package tui

import (
	"errors"
	"regexp"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/GhaziBenDahmane/harness/hosts/client"
)

// Custom commands: the markdown prompt templates and skills a server offers
// through CommandBackend. Each becomes a slash command; running one asks the
// server to expand it and sends the result as a normal user message. A name
// that a builtin already has stays the builtin's.

func init() {
	register(command{name: "reload", help: "reload the custom commands", run: func(m *Model, _ string) tea.Cmd { return m.loadCommands(true) }})
}

var (
	userMu      sync.RWMutex
	userCmds    []command
	userSkipped []string // "name (reason)", for /help
	reCmdName   = regexp.MustCompile(`^[a-z][a-z-]*$`)
)

// withUserCommands appends the custom commands to the builtins.
func withUserCommands(base []command) []command {
	userMu.RLock()
	defer userMu.RUnlock()
	if len(userCmds) == 0 {
		return base
	}
	return append(base[:len(base):len(base)], userCmds...)
}

// helpExtra is the part of /help about custom commands the builtins shadowed.
func helpExtra() string {
	userMu.RLock()
	defer userMu.RUnlock()
	if len(userSkipped) == 0 {
		return ""
	}
	return "not available as commands: " + strings.Join(userSkipped, ", ") + "\n"
}

// setUserCommands replaces the custom commands and reports how many it kept.
func setUserCommands(cs []client.Command) int {
	var out []command
	var skipped []string
	seen := map[string]bool{}
	for _, c := range cs {
		name := c.Name
		switch {
		case !reCmdName.MatchString(name):
			skipped = append(skipped, name+" (name cannot be typed as /command)")
			continue
		case seen[name]:
			continue
		case isBuiltin(name):
			skipped = append(skipped, name+" (a builtin has this name)")
			continue
		}
		seen[name] = true
		help := c.Description
		if c.Source != "" {
			help += " [" + c.Source + "]"
		}
		out = append(out, command{name: name, args: c.ArgHint, help: strings.TrimSpace(help),
			run: func(m *Model, arg string) tea.Cmd { return m.runCustom(name, arg) }})
	}
	userMu.Lock()
	userCmds, userSkipped = out, skipped
	userMu.Unlock()
	return len(out)
}

func isBuiltin(name string) bool {
	for _, c := range registry {
		if c.matches(name) {
			return true
		}
	}
	return false
}

// commandsMsg: the custom commands were fetched. loud is set by /reload, which answers even when there is nothing to say.
type commandsMsg struct {
	list []client.Command
	err  error
	loud bool
}

func (m *Model) loadCommands(loud bool) tea.Cmd {
	cb, ok := m.be.(CommandBackend)
	if !ok {
		if loud {
			m.notice("/reload: not supported by this backend/server")
		}
		return nil
	}
	return func() tea.Msg {
		list, err := cb.Commands(m.ctx)
		return commandsMsg{list, err, loud}
	}
}

func (c commandsMsg) apply(m *Model) tea.Cmd {
	switch {
	case errors.Is(c.err, client.ErrNotSupported):
		if c.loud {
			m.notice("/reload: not supported by this backend/server")
		}
	case c.err != nil:
		m.notice("custom commands: %v", c.err)
	default:
		n := setUserCommands(c.list)
		if c.loud {
			m.notice("%s loaded", plural(n, "custom command"))
		}
	}
	return nil
}

// expandedMsg: the server expanded a custom command into a prompt.
type expandedMsg struct {
	name, args, prompt string
	err                error
}

func (m *Model) runCustom(name, args string) tea.Cmd {
	if m.busy("/" + name) {
		return nil
	}
	cb, ok := m.be.(CommandBackend)
	if !ok {
		m.notice("/%s: not supported by this backend/server", name)
		return nil
	}
	return func() tea.Msg {
		p, err := cb.ExpandCommand(m.ctx, name, args)
		return expandedMsg{name, args, p, err}
	}
}

func (e expandedMsg) apply(m *Model) tea.Cmd {
	shown := strings.TrimSpace("/" + e.name + " " + e.args)
	switch {
	case e.err != nil:
		if !m.unsupported(shown, e.err) {
			m.notice("/%s: %v", e.name, e.err)
		}
		return nil
	case m.running || m.starting || m.pending != nil:
		m.notice("/%s: a run started meanwhile — send it again when it is over", e.name)
		return nil
	}
	m.add(block{kind: blockUser, text: shown})
	m.starting = true
	return m.start(func() (string, error) { return m.be.Send(m.ctx, e.prompt) })
}
