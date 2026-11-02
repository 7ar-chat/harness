package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
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
	// KeyFrom says where a key that is not stored comes from ("env OPENAI_API_KEY"); "" for none.
	KeyFrom string
	// NoKey marks a provider that needs none (an endpoint on this machine).
	NoKey bool
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
		return m.connectProvider(arg)
	}
	items := make([]pickItem, 0, 8)
	_, prov := m.provisioner()
	for _, p := range c.Providers() {
		desc := p.BaseURL
		switch {
		case prov:
			desc = providerDesc(p)
		case p.HasKey:
			desc += " · key stored"
		default:
			desc += " · no key"
		}
		items = append(items, pickItem{id: p.Name, label: p.Name, desc: strings.TrimPrefix(desc, " · ")})
	}
	pk := &picker{title: "Connect a provider", items: items, custom: true, hint: "pick one, or type a new provider name · enter · esc",
		pick: func(m *Model, it pickItem) tea.Cmd { m.openKeyPrompt(it.id); return nil }}
	if prov {
		pk.title = "Step 1 of 3 · pick a provider"
		pk.hint = "↑↓ · enter picks · esc cancels"
		pk.custom = false
		pk.pick = func(m *Model, it pickItem) tea.Cmd { return m.connectProvider(it.id) }
	}
	m.picker = pk
	return nil
}

// openKeyPrompt asks for the key of provider in a masked, single-line prompt.
func (m *Model) openKeyPrompt(provider string) {
	var key []rune
	title := "API key for " + provider
	if _, ok := m.provisioner(); ok {
		title = "Step 2 of 3 · API key for " + provider
	}
	p := &picker{title: title, hint: "paste the key · enter saves · esc cancels",
		items: []pickItem{{id: provider, label: "the key is hidden and stored in the credentials file only"}}}
	show := func() { p.footer = styleBold.Render("key: ") + strings.Repeat("•", min(len(key), 48)) + "▏" }
	show()
	p.extra = func(m *Model, k tea.KeyPressMsg) (tea.Cmd, bool) {
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
		case k.Text != "":
			key = append(key, []rune(k.Text)...)
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
	if _, ok := m.provisioner(); ok && c.err == nil {
		m.notice("saved the key of %s", c.provider)
		return m.listModels(c.provider) // step 3: no restart needed
	}
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
