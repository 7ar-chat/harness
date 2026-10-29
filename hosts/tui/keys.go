package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/lipgloss"

	tea "github.com/charmbracelet/bubbletea"
)

// leaderTag marks, in a keybind value, a key that follows the leader: "<leader>n".
const leaderTag = "<leader>"

// defaultKeys are the keys of the actions of the interface (tui.json "keybinds"
// overrides them one by one). Slash commands are bound under "command.<name>":
// "<leader>n" runs it after the leader key, any other key runs it directly.
var defaultKeys = map[string][]string{
	"leader":         {DefaultLeader},
	"palette":        {"ctrl+p"},
	"send":           {"enter"},
	"cancel":         {"esc"},
	"quit":           {"ctrl+c"},
	"tools":          {"ctrl+o"},
	"page_up":        {"pgup"},
	"page_down":      {"pgdown"},
	"half_page_up":   {"ctrl+u"},
	"half_page_down": {"ctrl+d"},
	"first":          {"ctrl+g", "home"},
	"last":           {"end"},
}

// remapped are the actions the key handler of model.go still switches on by
// their default key: canon translates a user key to it.
var remapped = []string{"send", "cancel", "quit", "tools"}

// knownAction reports whether name can appear under "keybinds".
func knownAction(name string) bool {
	if _, ok := defaultKeys[name]; ok {
		return true
	}
	if c, ok := strings.CutPrefix(name, "command."); ok {
		_, found := lookup(c)
		return found
	}
	return false
}

// normKey lowercases a key name and maps the usual aliases to Bubble Tea's.
func normKey(k string) string {
	k = strings.ToLower(strings.TrimSpace(k))
	switch k {
	case "escape":
		k = "esc"
	case "return":
		k = "enter"
	case "pageup":
		k = "pgup"
	case "pagedown":
		k = "pgdown"
	}
	return k
}

// Keymap maps action names to keys. Build it with NewKeymap.
type Keymap struct {
	keys    map[string][]string
	timeout time.Duration
	byKey   map[string]string // key -> action
}

// NewKeymap merges the user's keybinds over the defaults; leader and
// timeoutMS (milliseconds) are the "leader" and "leader_timeout" settings.
func NewKeymap(c TUIConfig) *Keymap {
	km := &Keymap{keys: map[string][]string{}, byKey: map[string]string{}}
	for a, ks := range defaultKeys {
		km.keys[a] = ks
	}
	for a, ks := range c.Keybinds {
		km.keys[a] = ks
	}
	if c.Leader != "" {
		km.keys["leader"] = []string{c.Leader}
	}
	km.timeout = time.Duration(c.LeaderTimeout) * time.Millisecond
	if km.timeout <= 0 {
		km.timeout = DefaultLeaderTimeout * time.Millisecond
	}
	for a, ks := range km.keys {
		if strings.HasPrefix(a, "command.") {
			continue
		}
		for _, k := range ks {
			km.byKey[k] = a
		}
	}
	return km
}

// Leader is the leader key.
func (km *Keymap) Leader() string { return km.keys["leader"][0] }

// action returns the action bound to the key, "" for none.
func (km *Keymap) action(key string) string { return km.byKey[key] }

// canon translates a key bound to send, cancel, quit or tools into the default
// key the handler switches on; a default key the user gave away to something
// else maps to "" so it no longer acts.
func (km *Keymap) canon(key string) string {
	if a := km.byKey[key]; a != "" {
		for _, r := range remapped {
			if a == r {
				return defaultKeys[a][0]
			}
		}
		return key
	}
	for _, r := range remapped {
		if defaultKeys[r][0] == key {
			return "unbound"
		}
	}
	return key
}

// leaderKey is the key after the leader that runs the command ("" when none).
func (km *Keymap) leaderKey(c command) string {
	for _, k := range km.cmdKeys(c) {
		if s, ok := strings.CutPrefix(k, leaderTag); ok {
			return s
		}
	}
	return ""
}

// cmdKeys is the keys the user gave the command, else its default leader bind.
func (km *Keymap) cmdKeys(c command) []string {
	if ks, ok := km.keys["command."+c.name]; ok {
		return ks
	}
	if c.bind != "" {
		return []string{leaderTag + c.bind}
	}
	return nil
}

// keyLabel shows a command's keys for /help and the palette: "ctrl+x n".
func (km *Keymap) keyLabel(c command) string {
	var out []string
	for _, k := range km.cmdKeys(c) {
		if s, ok := strings.CutPrefix(k, leaderTag); ok {
			k = km.Leader() + " " + s
		}
		out = append(out, k)
	}
	return strings.Join(out, ", ")
}

// leaderState is the pending leader key.
type leaderState struct {
	on  bool
	seq int
	at  time.Time
}

// leaderExpireMsg ends the leader sequence number seq when nothing followed.
type leaderExpireMsg struct{ seq int }

// uiState is what the foundation adds to Model: keys, leader sequence, config.
type uiState struct {
	keys    *Keymap
	lead    leaderState
	uiStore *stateFile // tui-state.json; nil keeps nothing
}

// initUI applies the configuration: keymap, theme, scroll speed, cursor.
func (m *Model) initUI(o Options) {
	cfg := DefaultTUIConfig()
	if o.Config != nil {
		cfg = *o.Config
	}
	m.keys = NewKeymap(cfg)
	m.uiStore = newStateFile(o.StateDir)
	th := themes[0]
	var saved string
	_ = m.uiStore.get("theme", &saved)
	for _, name := range []string{saved, cfg.Theme} {
		if t, ok := themeByName(name); ok && name != "" {
			th = t
			break
		}
	}
	applyTheme(th)
	m.vp.MouseWheelDelta = cfg.ScrollSpeed
	if !cfg.CursorBlink {
		m.in.Cursor.SetMode(cursor.CursorStatic)
	}
	if cfg.CursorStyle == "underline" {
		m.in.Cursor.Style = lipgloss.NewStyle().Underline(true)
	}
	if len(cfg.Notices) > 0 {
		m.add(block{kind: blockNotice, text: strings.Join(cfg.Notices, "\n")})
	}
}

// navKey handles the keys of the keymap that are not text: the leader sequence,
// the palette, scrolling and keys bound directly to a command.
func (m *Model) navKey(k tea.KeyMsg) (tea.Cmd, bool) {
	s := k.String()
	if m.lead.on {
		expired := m.now().Sub(m.lead.at) > m.keys.timeout
		m.lead.on = false
		switch {
		case expired:
			// the sequence timed out: the key is an ordinary key again
		case s == "esc" || s == m.keys.Leader():
			return nil, true
		default:
			for _, c := range commands() {
				if m.keys.leaderKey(c) == s {
					return m.runCommand(c), true
				}
			}
			m.add(block{kind: blockNotice, text: fmt.Sprintf("no command on %s %s", m.keys.Leader(), s)})
			return nil, true
		}
	}
	switch m.keys.action(s) {
	case "leader":
		m.lead = leaderState{on: true, seq: m.lead.seq + 1, at: m.now()}
		seq := m.lead.seq
		return tea.Tick(m.keys.timeout, func(time.Time) tea.Msg { return leaderExpireMsg{seq} }), true
	case "palette":
		return m.openPalette(), true
	case "page_up":
		m.vp.ViewUp()
		return nil, true
	case "page_down":
		m.vp.ViewDown()
		return nil, true
	case "half_page_up":
		m.vp.HalfViewUp()
		return nil, true
	case "half_page_down":
		m.vp.HalfViewDown()
		return nil, true
	case "first", "last":
		if (s == "home" || s == "end") && m.in.Value() != "" {
			return nil, false // the input box moves its cursor
		}
		if m.keys.action(s) == "first" {
			m.vp.GotoTop()
		} else {
			m.vp.GotoBottom()
		}
		return nil, true
	}
	for _, c := range commands() {
		for _, ck := range m.keys.cmdKeys(c) {
			if ck == s {
				return m.runCommand(c), true
			}
		}
	}
	return nil, false
}

// leaderExpire ends the leader sequence when it is still the one the timer was set for.
func (m *Model) leaderExpire(e leaderExpireMsg) {
	if m.lead.on && m.lead.seq == e.seq {
		m.lead.on = false
	}
}

// runCommand runs a command chosen by key or from the palette. A command that
// takes an argument is put in the input as "/name " to complete; the draft in
// the input is kept otherwise.
func (m *Model) runCommand(c command) tea.Cmd {
	if c.args != "" {
		m.in.SetValue("/" + c.name + " ")
		m.in.CursorEnd()
		return nil
	}
	return c.run(m, "")
}

// whichKey is the hint shown above the input after the leader key: the keys
// that can follow and the commands they run.
func (m *Model) whichKey(width int) string {
	lines := []string{styleBold.Render(m.keys.Leader()) + styleDim.Render("  press a key · esc cancels")}
	for _, c := range commands() {
		if k := m.keys.leaderKey(c); k != "" {
			lines = append(lines, clip(fmt.Sprintf("  %-4s %s", k, "/"+c.name+"  "+styleDim.Render(c.help)), width))
		}
	}
	return strings.Join(lines, "\n")
}
