package tui

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// First-run setup. A Connector that is also a Provisioner can switch the running
// engine to another provider and model, so /connect needs no restart: the home
// screen of an unconfigured harness explains the three steps (pick a provider,
// paste a key, choose a model) and enter starts them.

// Provisioner is a Connector that can also apply the choice at once.
type Provisioner interface {
	Connector
	// Configured reports whether the active provider has a usable key (or needs none).
	Configured() bool
	// Active is the provider and model in use ("" when none is configured).
	Active() (provider, model string)
	// Models lists the models provider serves, using its stored key; on an error
	// the list may still hold the models the configuration names.
	Models(ctx context.Context, provider string) ([]string, error)
	// Apply makes provider and model the engine of the next runs.
	Apply(ctx context.Context, provider, model string) error
	// ConfigPath is the file a user edits to add a provider ("" when unknown).
	ConfigPath() string
}

func (m *Model) provisioner() (Provisioner, bool) {
	p, ok := m.ext.connector.(Provisioner)
	return p, ok
}

// needsSetup says whether no provider is usable yet.
func (m *Model) needsSetup() bool {
	p, ok := m.provisioner()
	return ok && !p.Configured()
}

// homeScreen is the screen of an unconfigured harness: what it is, what to do
// first, and where the other ways of configuring it are.
func (m *Model) homeScreen() string {
	var b strings.Builder
	b.WriteString(styleUser.Render("harness") + styleDim.Render("  a coding agent in your terminal") + "\n\n")
	b.WriteString("  Welcome! Connect an AI provider to start. It takes about a minute.\n\n")
	step := func(n, title, desc string) {
		b.WriteString("    " + styleToolHdr.Render(n) + "  " + styleBold.Render(title) + "\n")
		b.WriteString("       " + styleDim.Render(desc) + "\n")
	}
	step("1", "Pick a provider", "OpenAI, Anthropic, or any OpenAI-compatible service")
	step("2", "Paste your API key", "kept in your user folder only, readable by you alone")
	step("3", "Choose a model", "harness asks the provider which ones your key can use")
	b.WriteString("\n  " + styleUser.Render("▸ Press enter") + " to start, or type " + styleBold.Render("/connect") + "\n\n")
	b.WriteString(styleDim.Render("  Already have a key in your environment?") + "\n")
	b.WriteString(styleDim.Render("    export OPENAI_API_KEY=…  or  ANTHROPIC_API_KEY=…  then run harness again.") + "\n")
	b.WriteString(styleDim.Render("  Running a local model (Ollama, llama.cpp)?") + "\n")
	if p, ok := m.provisioner(); ok && p.ConfigPath() != "" {
		b.WriteString(styleDim.Render("    add it under providers: in "+tildePath(p.ConfigPath())+" — no key needed.") + "\n")
	} else {
		b.WriteString(styleDim.Render("    add it under providers: in your config.yml — no key needed.") + "\n")
	}
	return b.String()
}

// providerDesc is the line under a provider in the picker.
func providerDesc(p ProviderInfo) string {
	var parts []string
	if p.BaseURL != "" {
		parts = append(parts, p.BaseURL)
	}
	switch {
	case p.NoKey:
		parts = append(parts, "no key needed")
	case p.KeyFrom != "":
		parts = append(parts, "key from "+p.KeyFrom)
	case p.HasKey:
		parts = append(parts, "key stored")
	default:
		parts = append(parts, "needs a key")
	}
	return strings.Join(parts, " · ")
}

// connectStep is the step after a provider is picked: the key when it lacks
// one, else the model.
func (m *Model) connectProvider(name string) tea.Cmd {
	if p, ok := m.provisioner(); ok {
		for _, pi := range p.Providers() {
			if pi.Name == name && (pi.HasKey || pi.NoKey || pi.KeyFrom != "") {
				return m.listModels(name)
			}
		}
	}
	m.openKeyPrompt(name)
	return nil
}

// modelsForMsg is the model list of a provider being connected.
type modelsForMsg struct {
	provider string
	ids      []string
	err      error
}

// listModels asks the provider which models it serves, then opens the picker.
func (m *Model) listModels(provider string) tea.Cmd {
	p, ok := m.provisioner()
	if !ok {
		return nil
	}
	m.notice("asking %s which models you can use…", provider)
	ctx := m.ctx
	return func() tea.Msg {
		ids, err := p.Models(ctx, provider)
		return modelsForMsg{provider, ids, err}
	}
}

func (r modelsForMsg) apply(m *Model) tea.Cmd {
	cur := ""
	if p, ok := m.provisioner(); ok {
		if ap, am := p.Active(); ap == r.provider {
			cur = am
		}
	}
	items := make([]pickItem, 0, len(r.ids))
	for _, id := range r.ids {
		items = append(items, pickItem{id: id, label: id, current: id == cur})
	}
	hint := "type to filter · ↑↓ · enter picks · esc cancels"
	if r.err != nil {
		m.notice("could not list the models of %s (%v)", r.provider, r.err)
		hint = "type the model id and press enter · esc cancels"
	}
	provider := r.provider
	pk := &picker{title: "Choose a model for " + provider, items: items, custom: true, hint: hint,
		pick: func(m *Model, it pickItem) tea.Cmd { return m.applyConnection(provider, it.id) }}
	for i, it := range items {
		if it.current {
			pk.sel = i
		}
	}
	m.picker = pk
	return nil
}

// appliedMsg: Apply finished.
type appliedMsg struct {
	provider, model string
	err             error
}

func (m *Model) applyConnection(provider, model string) tea.Cmd {
	p, ok := m.provisioner()
	if !ok {
		return nil
	}
	m.notice("connecting to %s…", provider)
	ctx := m.ctx
	return func() tea.Msg { return appliedMsg{provider, model, p.Apply(ctx, provider, model)} }
}

func (a appliedMsg) apply(m *Model) tea.Cmd {
	if a.err != nil {
		m.notice("could not connect to %s: %v", a.provider, a.err)
		return nil
	}
	if mb, ok := m.be.(ModelBackend); ok {
		m.useModel(mb, a.model)
	}
	m.notice("%s", fmt.Sprintf("connected to %s · model %s — type a message to begin", a.provider, a.model))
	return nil
}
