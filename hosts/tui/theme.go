package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"

	tea "charm.land/bubbletea/v2"
)

// Theme names the colors of the interface. A color is an ANSI index ("6"), a
// hex value ("#88c0d0") or "" for the terminal default. Tests and pipes have
// no color profile, so every theme renders as plain text there.
type Theme struct {
	Name                                                   string
	User, Dim, Err, OK, Add, Del, Hunk, Warn, Status, Code string
	Accent, Selection                                      string
	// AddBg/DelBg tint whole added/removed lines in diffs; AddHi/DelHi the changed words. "" draws colored text only.
	AddBg, DelBg, AddHi, DelHi string
	// PanelBg tints the messages of the user; "" draws them without a panel.
	PanelBg string
}

// The styles of the transcript, status line and pickers; applyTheme assigns them.
var (
	styleUser, styleDim, styleBold, styleCode, styleErr, styleOK lipgloss.Style
	styleAdd, styleDel, styleHunk, styleWarn, styleStatus        lipgloss.Style
	styleToolHdr, styleSel                                       lipgloss.Style
	styleHead, styleLink, styleKw, styleStr, styleNum, styleFn   lipgloss.Style
)

// curTheme is the name of the applied theme.
var curTheme = "default"

// diffTint holds the background colors of the applied theme for diffs.
var diffTint struct{ add, del, addHi, delHi, panel string }

// themes in the order /themes lists them; the first is the default (ANSI colors,
// so it follows the terminal's palette).
var themes = []Theme{
	{Name: "default", User: "6", Dim: "8", Err: "1", OK: "2", Add: "2", Del: "1", Hunk: "6", Warn: "3", Code: "3", Selection: "6"},
	{Name: "dark", User: "#7dcfff", Dim: "#6b7089", Err: "#f7768e", OK: "#9ece6a", Add: "#9ece6a", Del: "#f7768e", Hunk: "#7aa2f7", Warn: "#e0af68", Status: "#3b4261", Code: "#e0af68", Accent: "#bb9af7", Selection: "#7dcfff", AddBg: "#1f2d24", DelBg: "#33232b", AddHi: "#2f5a3a", DelHi: "#6a2f3f", PanelBg: "#1e2030"},
	{Name: "light", User: "#0b6e99", Dim: "#6e7781", Err: "#cf222e", OK: "#1a7f37", Add: "#1a7f37", Del: "#cf222e", Hunk: "#0969da", Warn: "#9a6700", Status: "#d0d7de", Code: "#953800", Accent: "#8250df", Selection: "#0969da", AddBg: "#dafbe1", DelBg: "#ffebe9", AddHi: "#aceebb", DelHi: "#ffc1c0", PanelBg: "#eaeef2"},
	{Name: "high-contrast", User: "14", Dim: "7", Err: "9", OK: "10", Add: "10", Del: "9", Hunk: "14", Warn: "11", Status: "15", Code: "11", Accent: "13", Selection: "15"},
	{Name: "solarized", User: "#2aa198", Dim: "#586e75", Err: "#dc322f", OK: "#859900", Add: "#859900", Del: "#dc322f", Hunk: "#268bd2", Warn: "#b58900", Status: "#073642", Code: "#cb4b16", Accent: "#6c71c4", Selection: "#2aa198", AddBg: "#0b3b36", DelBg: "#3b2a2c", AddHi: "#14584f", DelHi: "#5c3436", PanelBg: "#073642"},
	{Name: "nord", User: "#88c0d0", Dim: "#616e88", Err: "#bf616a", OK: "#a3be8c", Add: "#a3be8c", Del: "#bf616a", Hunk: "#81a1c1", Warn: "#ebcb8b", Status: "#434c5e", Code: "#d08770", Accent: "#b48ead", Selection: "#88c0d0", AddBg: "#35433f", DelBg: "#46363c", AddHi: "#4d6657", DelHi: "#6b4650", PanelBg: "#3b4252"},
	{Name: "gruvbox", User: "#8ec07c", Dim: "#928374", Err: "#fb4934", OK: "#b8bb26", Add: "#b8bb26", Del: "#fb4934", Hunk: "#83a598", Warn: "#fabd2f", Status: "#504945", Code: "#fe8019", Accent: "#d3869b", Selection: "#8ec07c", AddBg: "#34382b", DelBg: "#402b27", AddHi: "#4d5a30", DelHi: "#6a3a31", PanelBg: "#3c3836"},
}

func init() {
	applyTheme(themes[0])
	register(command{name: "themes", aliases: []string{"theme"}, args: "[name]", help: "pick a color theme (live preview)", run: (*Model).cmdThemes})
}

// themeByName finds a theme; the match ignores case.
func themeByName(name string) (Theme, bool) {
	for _, t := range themes {
		if strings.EqualFold(t.Name, strings.TrimSpace(name)) {
			return t, true
		}
	}
	return Theme{}, false
}

func fg(c string) lipgloss.Style {
	s := lipgloss.NewStyle()
	if c == "" {
		return s
	}
	return s.Foreground(lipgloss.Color(c))
}

// colorsOn says whether the interface draws colors. It is on; the tests turn it
// off so the screens they compare are plain text (Bubble Tea itself downsamples
// to the terminal's color support).
var colorsOn = true

// applyTheme points the package styles at the theme's colors.
func applyTheme(t Theme) {
	if !colorsOn {
		p := lipgloss.NewStyle()
		styleUser, styleDim, styleBold, styleCode, styleErr, styleOK = p, p, p, p, p, p
		styleAdd, styleDel, styleHunk, styleWarn, styleStatus = p, p, p, p, p
		styleToolHdr, styleSel, styleHead, styleLink, styleKw, styleStr, styleNum, styleFn = p, p, p, p, p, p, p, p
		diffTint.add, diffTint.del, diffTint.addHi, diffTint.delHi, diffTint.panel = "", "", "", "", ""
		curTheme = t.Name
		return
	}
	t = withAdaptiveTints(t)
	styleUser = fg(t.User).Bold(true)
	styleDim = fg(t.Dim)
	styleBold = lipgloss.NewStyle().Bold(true)
	styleCode = fg(t.Code)
	styleErr = fg(t.Err)
	styleOK = fg(t.OK)
	styleAdd = fg(t.Add)
	styleDel = fg(t.Del)
	styleHunk = fg(t.Hunk)
	styleWarn = fg(t.Warn).Bold(true)
	if strings.HasPrefix(t.Status, "#") {
		// a hex theme paints the bar's background and keeps the terminal's text color readable on it
		styleStatus = lipgloss.NewStyle().Background(lipgloss.Color(t.Status))
	} else {
		styleStatus = fg(t.Status).Reverse(true)
	}
	styleToolHdr = fg(t.Accent).Bold(true)
	styleSel = fg(t.Selection).Bold(true)
	styleHead = fg(orColor(t.Accent, t.User)).Bold(true)
	styleLink = fg(t.Hunk).Underline(true)
	styleKw = fg(orColor(t.Accent, "5"))
	styleStr = fg(orColor(t.Add, "2"))
	styleNum = fg(orColor(t.Warn, "3"))
	styleFn = fg(orColor(t.Hunk, "4"))
	diffTint.add, diffTint.del, diffTint.addHi, diffTint.delHi, diffTint.panel = t.AddBg, t.DelBg, t.AddHi, t.DelHi, t.PanelBg
	curTheme = t.Name
}

func (m *Model) cmdThemes(arg string) tea.Cmd {
	if arg != "" {
		t, ok := themeByName(arg)
		if !ok {
			m.add(block{kind: blockNotice, text: fmt.Sprintf("unknown theme %q — /themes lists them", arg)})
			return nil
		}
		m.setTheme(t)
		return nil
	}
	prev := curTheme
	items := make([]pickItem, len(themes))
	sel := 0
	for i, t := range themes {
		items[i] = pickItem{id: t.Name, label: t.Name, current: t.Name == prev}
		if t.Name == prev {
			sel = i
		}
	}
	show := func(it pickItem) {
		if t, ok := themeByName(it.id); ok {
			applyTheme(t)
		}
	}
	m.picker = &picker{
		title:   "theme",
		items:   items,
		sel:     sel,
		preview: show,
		cancel:  func() { show(pickItem{id: prev}) },
		pick:    func(m *Model, it pickItem) tea.Cmd { t, _ := themeByName(it.id); m.setTheme(t); return nil },
	}
	return nil
}

// setTheme applies the theme and remembers the choice in the state file.
func (m *Model) setTheme(t Theme) {
	applyTheme(t)
	if err := m.uiStore.set("theme", t.Name); err != nil {
		m.add(block{kind: blockNotice, text: "theme: could not save the choice: " + err.Error()})
	}
}

// orColor is c, or def when c is empty.
func orColor(c, def string) string {
	if c == "" {
		return def
	}
	return c
}

// spinFrames are the frames of the busy spinner of the status line.
var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// styleInput colors the prompt bar, placeholder and current line of the input with
// the theme; it runs when the theme changed since the last call.
func (m *Model) styleInput() {
	if m.inputTheme == curTheme {
		return
	}
	m.inputTheme = curTheme
	bar := fg(orColor(curAccent(), "6")).Bold(true)
	st := m.in.Styles()
	for _, s := range []*textarea.StyleState{&st.Focused, &st.Blurred} {
		s.Prompt = bar
		s.Placeholder = styleDim
		s.CursorLine = lipgloss.NewStyle()
		s.Text = lipgloss.NewStyle()
	}
	st.Blurred.Prompt = styleDim
	m.in.SetStyles(st)
}

// curAccent is the accent color of the applied theme.
func curAccent() string {
	if t, ok := themeByName(curTheme); ok {
		return orColor(t.Accent, t.User)
	}
	return ""
}

// darkBackground says whether the terminal background is dark (assumed until the
// terminal answers the background-color query).
var darkBackground = true

// withAdaptiveTints gives a theme without its own diff tints (the ANSI themes,
// which follow the terminal's palette) tints matching the terminal background.
func withAdaptiveTints(t Theme) Theme {
	if t.AddBg != "" {
		return t
	}
	if darkBackground {
		t.AddBg, t.DelBg, t.AddHi, t.DelHi = "#14301f", "#3a1a22", "#1f5a35", "#7a2a3a"
	} else {
		t.AddBg, t.DelBg, t.AddHi, t.DelHi = "#dafbe1", "#ffebe9", "#aceebb", "#ffc1c0"
	}
	return t
}

// setBackground reacts to the terminal's answer about its background color.
func (m *Model) setBackground(dark bool) {
	if dark == darkBackground {
		return
	}
	darkBackground = dark
	if t, ok := themeByName(curTheme); ok {
		applyTheme(t)
	}
}
