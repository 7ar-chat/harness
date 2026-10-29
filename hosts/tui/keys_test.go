package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// newConfigured is newModel with a tui.json (text) and a state file.
func newConfigured(t *testing.T, cfgText, stateDir string) (*Model, *time.Time) {
	t.Helper()
	t.Cleanup(func() { applyTheme(themes[0]) })
	cfg := ParseTUIConfig([]byte(cfgText))
	now := t0
	m := New(&fakeBackend{}, Options{Now: func() time.Time { return now }, StaticCursor: true, Config: &cfg, StateDir: stateDir})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return m, &now
}

func TestLeaderThenBindRunsTheCommand(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	press(m, tea.KeyCtrlX)
	typeText(m, "q")

	if !m.quit {
		t.Fatal("leader+q did not quit")
	}
}

func TestLeaderShowsWhichKeyUntilEsc(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	press(m, tea.KeyCtrlX)
	hint := m.View()
	press(m, tea.KeyEsc)

	if !strings.Contains(hint, "esc cancels") || !strings.Contains(hint, "/quit") || !strings.Contains(hint, "/palette") {
		t.Fatalf("no which-key hint:\n%s", hint)
	}
	if strings.Contains(m.View(), "esc cancels") {
		t.Fatal("hint still shown after esc")
	}
}

func TestLeaderTimesOutBySequenceAndByClock(t *testing.T) {
	m, now := newModel(&fakeBackend{})

	press(m, tea.KeyCtrlX)
	update(m, leaderExpireMsg{seq: m.lead.seq})
	typeText(m, "q")
	if m.quit || m.in.Value() != "q" {
		t.Fatalf("a key after the timeout must type (quit=%v, input=%q)", m.quit, m.in.Value())
	}

	m.in.Reset()
	press(m, tea.KeyCtrlX)
	*now = now.Add(3 * time.Second)
	typeText(m, "q")
	if m.quit || m.in.Value() != "q" {
		t.Fatalf("a late key must type (quit=%v, input=%q)", m.quit, m.in.Value())
	}
}

func TestStaleTimerDoesNotCancelANewLeader(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	press(m, tea.KeyCtrlX)
	old := m.lead.seq
	press(m, tea.KeyEsc)
	press(m, tea.KeyCtrlX)
	update(m, leaderExpireMsg{seq: old})

	if !m.lead.on {
		t.Fatal("the timer of an earlier sequence ended the new one")
	}
}

func TestUnknownSecondKeyIsReported(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	press(m, tea.KeyCtrlX)
	typeText(m, "z")

	if m.lead.on || !strings.Contains(m.View(), "no command on ctrl+x z") {
		t.Fatalf("unknown key not reported:\n%s", m.View())
	}
}

func TestPaletteListsAllCommandsAndRunsOne(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	press(m, tea.KeyCtrlP)
	v := m.View()
	for _, c := range commands() {
		if !strings.Contains(v, "/"+c.name) && len(commands()) <= pickRows {
			t.Fatalf("palette misses /%s:\n%s", c.name, v)
		}
	}
	typeText(m, "quit")
	v = m.View()
	press(m, tea.KeyEnter)

	if !strings.Contains(v, "[ctrl+x q]") {
		t.Fatalf("palette does not show the bind:\n%s", v)
	}
	if !m.quit {
		t.Fatal("enter did not run /quit")
	}
}

func TestPaletteCommandWithArgsFillsTheInput(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	press(m, tea.KeyCtrlP)
	typeText(m, "themes")
	press(m, tea.KeyEnter)

	if m.picker != nil || m.in.Value() != "/themes " {
		t.Fatalf("picker=%v input=%q", m.picker, m.in.Value())
	}
}

func TestLeaderPOpensThePalette(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	press(m, tea.KeyCtrlX)
	typeText(m, "p")

	if m.picker == nil || m.picker.title != "commands" {
		t.Fatal("leader+p did not open the palette")
	}
}

func TestScrollKeys(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	for i := 0; i < 60; i++ {
		m.add(block{kind: blockNotice, text: "line"})
	}
	m.refresh()
	m.vp.GotoBottom()

	press(m, tea.KeyCtrlG)
	top := m.vp.AtTop()
	press(m, tea.KeyEnd)

	if !top || !m.vp.AtBottom() {
		t.Fatalf("ctrl+g top=%v, end bottom=%v", top, m.vp.AtBottom())
	}
}

func TestHomeEndMoveTheCursorWhenTyping(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	for i := 0; i < 60; i++ {
		m.add(block{kind: blockNotice, text: "line"})
	}
	m.refresh()
	typeText(m, "abc")

	press(m, tea.KeyHome)

	if m.vp.AtTop() {
		t.Fatal("home scrolled the transcript while a message was being typed")
	}
}

func TestConfigRebindsLeaderAndCommand(t *testing.T) {
	m, _ := newConfigured(t, `{"leader": "ctrl+b", "keybinds": {"palette": ["ctrl+k"], "command.quit": "<leader>x"}}`, "")

	press(m, tea.KeyCtrlK)
	opened := m.picker != nil
	press(m, tea.KeyEsc)
	press(m, tea.KeyCtrlX) // the old leader is now an ordinary key
	press(m, tea.KeyCtrlB)
	typeText(m, "q")
	stillHere := !m.quit
	press(m, tea.KeyCtrlB)
	typeText(m, "x")

	if !opened || !stillHere || !m.quit {
		t.Fatalf("palette=%v old-bind-ignored=%v quit=%v", opened, stillHere, m.quit)
	}
}

func TestConfigRebindsCancelToAnotherKey(t *testing.T) {
	m, _ := newConfigured(t, `{"keybinds": {"quit": "ctrl+q"}}`, "")

	press(m, tea.KeyCtrlC) // no longer quits
	press(m, tea.KeyCtrlC)
	notQuit := !m.quit
	update(m, tea.KeyMsg{Type: tea.KeyCtrlQ})
	update(m, tea.KeyMsg{Type: tea.KeyCtrlQ})

	if !notQuit || !m.quit {
		t.Fatalf("old ctrl+c quit=%v, then ctrl+q quit=%v", !notQuit, m.quit)
	}
}

func TestParseTUIConfigDefaults(t *testing.T) {
	c := ParseTUIConfig(nil)

	if c.Leader != "ctrl+x" || c.LeaderTimeout != 2000 || c.ScrollSpeed != 3 || !c.Mouse || !c.CursorBlink || c.DiffStyle != "auto" || len(c.Notices) != 0 {
		t.Fatalf("%+v", c)
	}
}

func TestParseTUIConfigReadsEveryFieldAndComments(t *testing.T) {
	c := ParseTUIConfig([]byte(`{
		// the look
		"theme": "nord", /* block */
		"leader": "Ctrl+B", "leader_timeout": 500, "scroll_speed": 7,
		"diff_style": "stacked", "mouse": false,
		"cursor": {"style": "underline", "blinking": false},
		"keybinds": {"palette": "ctrl+k, f1", "tools": ["ctrl+t"], "last": "//"},
	}`))

	if len(c.Notices) != 0 {
		t.Fatalf("notices: %v", c.Notices)
	}
	if c.Theme != "nord" || c.Leader != "ctrl+b" || c.LeaderTimeout != 500 || c.ScrollSpeed != 7 || c.DiffStyle != "stacked" || c.Mouse || c.CursorBlink || c.CursorStyle != "underline" {
		t.Fatalf("%+v", c)
	}
	if got := strings.Join(c.Keybinds["palette"], "|") + " " + c.Keybinds["last"][0]; got != "ctrl+k|f1 //" {
		t.Fatalf("keys %q", got)
	}
}

func TestParseTUIConfigInvalidValuesFallBackWithNotices(t *testing.T) {
	c := ParseTUIConfig([]byte(`{"theme": "neon", "leader_timeout": -1, "scroll_speed": "fast", "diff_style": "split", "mouse": 1,
		"cursor": {"style": "bar", "shape": 1}, "keybinds": {"palette": 5, "nope": "x"}, "extra": true}`))

	d := DefaultTUIConfig()
	if c.Theme != "" || c.LeaderTimeout != d.LeaderTimeout || c.ScrollSpeed != d.ScrollSpeed || c.DiffStyle != d.DiffStyle || !c.Mouse || c.CursorStyle != d.CursorStyle || c.Keybinds != nil {
		t.Fatalf("not defaults: %+v", c)
	}
	all := strings.Join(c.Notices, "\n")
	for _, want := range []string{"theme", "leader_timeout", "scroll_speed", "diff_style", "mouse", "cursor.style", "cursor.shape", "keybinds.palette", `"nope"`, `"extra"`} {
		if !strings.Contains(all, want) {
			t.Errorf("no notice about %s:\n%s", want, all)
		}
	}
}

func TestParseTUIConfigNotJSON(t *testing.T) {
	c := ParseTUIConfig([]byte(`{"theme": `))

	if c.Leader != "ctrl+x" || len(c.Notices) != 1 {
		t.Fatalf("%+v", c)
	}
}

func TestConfigNoticesAppearInTheTranscript(t *testing.T) {
	m, _ := newConfigured(t, `{"scroll_speed": 0}`, "")

	if v := m.View(); !strings.Contains(v, "scroll_speed") {
		t.Fatalf("no notice:\n%s", v)
	}
}

func TestThemeCommandAppliesAndPersistsMergingState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tui-state.json")
	if err := os.WriteFile(path, []byte(`{"other": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, _ := newConfigured(t, ``, dir)

	typeText(m, "/themes nord")
	press(m, tea.KeyEnter)

	data, _ := os.ReadFile(path)
	if curTheme != "nord" || !strings.Contains(string(data), `"theme": "nord"`) || !strings.Contains(string(data), `"other": 1`) {
		t.Fatalf("theme=%s state=%s", curTheme, data)
	}
	m2, _ := newConfigured(t, `{"theme": "gruvbox"}`, dir)
	_ = m2
	if curTheme != "nord" {
		t.Fatalf("the saved choice must win over tui.json, got %s", curTheme)
	}
}

func TestThemePickerPreviewsAndEscRestores(t *testing.T) {
	m, _ := newConfigured(t, ``, "")

	typeText(m, "/themes")
	press(m, tea.KeyEnter)
	press(m, tea.KeyDown)
	previewed := curTheme
	press(m, tea.KeyEsc)

	if previewed != themes[1].Name || curTheme != "default" || m.picker != nil {
		t.Fatalf("previewed=%s after esc=%s", previewed, curTheme)
	}
}

func TestThemeFromConfigAndUnknownThemeCommand(t *testing.T) {
	m, _ := newConfigured(t, `{"theme": "solarized"}`, "")
	if curTheme != "solarized" {
		t.Fatalf("theme %s", curTheme)
	}

	typeText(m, "/themes neon")
	press(m, tea.KeyEnter)

	if curTheme != "solarized" || !strings.Contains(m.View(), "unknown theme") {
		t.Fatalf("theme %s\n%s", curTheme, m.View())
	}
}

func TestEveryThemeRendersPlainWithoutColorProfile(t *testing.T) {
	for _, th := range themes {
		applyTheme(th)
		if got := styleErr.Render("boom") + styleStatus.Render("s") + styleSel.Render("x"); got != "boomsx" {
			t.Errorf("%s: %q", th.Name, got)
		}
	}
	applyTheme(themes[0])
}

func TestHelpShowsLeaderBinds(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	typeText(m, "/help")
	press(m, tea.KeyEnter)

	if v := m.View(); !strings.Contains(v, "[ctrl+x q]") || !strings.Contains(v, "ctrl+p command palette") {
		t.Fatalf("help lacks the binds:\n%s", v)
	}
}
