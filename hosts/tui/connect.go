package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// /connect: store the API key of a provider without leaving the UI. The TUI
// only collects the key (masked, never echoed or put in the transcript) and
// hands it to a Connector; where it is stored and how a running harness picks
// it up is the connector's business.

// ProviderInfo is a provider /connect offers.
type ProviderInfo struct {
	Name    string
	BaseURL string
	// HasKey is true when a key is stored for the provider already.
	HasKey bool
}

// Connector lists the providers and stores their keys.
type Connector interface {
	Providers() []ProviderInfo
	// Save stores key for provider. The error must not contain the key.
	Save(provider, key string) error
}

// RestartNotifier is implemented by a Connector whose saved key reaches only
// the next start of the program (an engine whose configuration is fixed once
// it runs); /connect then says "restart to apply".
type RestartNotifier interface{ RestartNeeded() bool }

func init() {
	register(command{name: "connect", args: "[provider]", help: "add a provider and its API key", run: (*Model).cmdConnect})
}

func (m *Model) cmdConnect(arg string) tea.Cmd {
	c := m.ext.connector
	if c == nil {
		m.notice("/connect: not available here — run: harness auth login <provider>")
		return nil
	}
	if arg != "" {
		m.openKeyPrompt(arg)
		return nil
	}
	items := make([]pickItem, 0, 8)
	for _, p := range c.Providers() {
		desc := p.BaseURL
		if p.HasKey {
			desc += " · key stored"
		} else {
			desc += " · no key"
		}
		items = append(items, pickItem{id: p.Name, label: p.Name, desc: strings.TrimPrefix(desc, " · ")})
	}
	m.picker = &picker{title: "Connect a provider", items: items, custom: true, hint: "pick one, or type a new provider name · enter · esc",
		pick: func(m *Model, it pickItem) tea.Cmd { m.openKeyPrompt(it.id); return nil }}
	return nil
}

// openKeyPrompt asks for the key of provider in a masked, single-line prompt.
func (m *Model) openKeyPrompt(provider string) {
	var key []rune
	p := &picker{title: "API key for " + provider, hint: "paste the key · enter saves · esc cancels",
		items: []pickItem{{id: provider, label: "the key is hidden and stored in the credentials file only"}}}
	show := func() { p.footer = styleBold.Render("key: ") + strings.Repeat("•", min(len(key), 48)) + "▏" }
	show()
	p.extra = func(m *Model, k tea.KeyMsg) (tea.Cmd, bool) {
		switch {
		case k.String() == "esc" || k.String() == "ctrl+c":
			clear(key) // the picker closes itself
			return nil, false
		case k.String() == "enter":
			text := strings.TrimSpace(string(key))
			clear(key)
			if text == "" {
				return nil, true
			}
			m.picker = nil
			c := m.ext.connector
			return func() tea.Msg { return connectedMsg{provider, c.Save(provider, text)} }, true
		case k.String() == "backspace":
			if len(key) > 0 {
				key = key[:len(key)-1]
			}
		case k.Type == tea.KeyRunes || k.Type == tea.KeySpace:
			key = append(key, k.Runes...)
		}
		show()
		return nil, true // nothing reaches the filter or the input box
	}
	m.picker = p
}

// connectedMsg: Save finished.
type connectedMsg struct {
	provider string
	err      error
}

func (c connectedMsg) apply(m *Model) tea.Cmd {
	switch rn, _ := m.ext.connector.(RestartNotifier); {
	case c.err != nil:
		m.notice("/connect: %v", c.err)
	case rn != nil && rn.RestartNeeded():
		m.notice("saved the key of %s — restart to apply (a running harness keeps its configuration)", c.provider)
	default:
		m.notice("%s", fmt.Sprintf("saved the key of %s — new runs use it", c.provider))
	}
	return nil
}
