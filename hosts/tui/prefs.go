package tui

import (
	"runtime"

	tea "github.com/charmbracelet/bubbletea"
)

// prefs is the state of the view preferences that can change while the UI
// runs (diff style, wheel scrolling, mouse, tips, the user-message jumps).
type prefs struct {
	diffStyle string // diffAuto or diffStacked
	wheel     accelerator
	mouse     bool
	tipsOff   bool // /tips hid the tip of the welcome screen
}

// stateTipsOff is the key of tui-state.json that remembers /tips.
const stateTipsOff = "tips_hidden"

func init() {
	register(
		command{name: "mouse", help: "toggle mouse support (wheel scrolling); off lets the terminal select text", run: (*Model).cmdMouse},
	)
	// The actions of the message navigation exist in the keymap but are unbound
	// by default (as in opencode); tui.json "keybinds" can give them keys.
	for _, a := range []string{actNextUser, actPrevUser, actLastUser} {
		defaultKeys[a] = nil
	}
	defaultKeys[actLineUp] = []string{"alt+ctrl+y"}
	defaultKeys[actLineDown] = []string{"alt+ctrl+e"}
}

func (m *Model) initPrefs(o Options) {
	cfg := DefaultTUIConfig()
	if o.Config != nil {
		cfg = *o.Config
	}
	m.pf = prefs{diffStyle: cfg.DiffStyle, mouse: cfg.Mouse}
	m.pf.wheel = accelerator{speed: cfg.ScrollSpeed, enabled: cfg.ScrollAccel, now: m.now}
	_ = m.uiStore.get(stateTipsOff, &m.pf.tipsOff)
	m.initAttention(cfg, o)
}

// prefKey handles the keys of this file's features before the generic ones:
// ctrl+z suspends, the line and message navigation actions of the keymap.
func (m *Model) prefKey(k tea.KeyMsg) (tea.Cmd, bool) {
	s := k.String()
	if s == "ctrl+z" && runtime.GOOS != "windows" {
		return tea.Suspend, true
	}
	switch {
	case m.keys.is(s, actLineUp):
		m.vp.ScrollUp(1)
	case m.keys.is(s, actLineDown):
		m.vp.ScrollDown(1)
	case m.keys.is(s, actNextUser):
		m.jumpUser(1)
	case m.keys.is(s, actPrevUser):
		m.jumpUser(-1)
	case m.keys.is(s, actLastUser):
		m.jumpUser(0)
	default:
		return nil, false
	}
	return nil, true
}

// is reports whether key is bound to the action (the order of "alt" and "ctrl"
// in a configured key does not matter).
func (km *Keymap) is(key, action string) bool {
	for _, k := range km.keys[action] {
		if k == key || canonMods(k) == canonMods(key) {
			return true
		}
	}
	return false
}

// canonMods writes "ctrl+alt+x" the way Bubble Tea names it, "alt+ctrl+x".
func canonMods(k string) string {
	if len(k) > 9 && k[:9] == "ctrl+alt+" {
		return "alt+ctrl+" + k[9:]
	}
	return k
}

// cmdMouse switches the mouse reporting of the terminal on or off.
func (m *Model) cmdMouse(string) tea.Cmd {
	m.pf.mouse = !m.pf.mouse
	if m.pf.mouse {
		m.notice("mouse: on (wheel scrolls the transcript)")
		return tea.EnableMouseCellMotion
	}
	m.notice("mouse: off (the terminal selects text)")
	return tea.DisableMouse
}
