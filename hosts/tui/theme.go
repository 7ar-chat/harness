package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	tea "github.com/charmbracelet/bubbletea"
)

// Theme names the colors of the interface. A color is an ANSI index ("6"), a
// hex value ("#88c0d0") or "" for the terminal default. Tests and pipes have
// no color profile, so every theme renders as plain text there.
type Theme struct {
	Name                                                   string
	User, Dim, Err, OK, Add, Del, Hunk, Warn, Status, Code string
	Accent, Selection                                      string
}

// The styles of the transcript, status line and pickers; applyTheme assigns them.
var (
	styleUser, styleDim, styleBold, styleCode, styleErr, styleOK lipgloss.Style
	styleAdd, styleDel, styleHunk, styleWarn, styleStatus        lipgloss.Style
	styleToolHdr, styleSel                                       lipgloss.Style
)

// curTheme is the name of the applied theme.
var curTheme = "default"

// themes in the order /themes lists them; the first is the default (ANSI colors,
// so it follows the terminal's palette).
var themes = []Theme{
	{Name: "default", User: "6", Dim: "8", Err: "1", OK: "2", Add: "2", Del: "1", Hunk: "6", Warn: "3", Code: "3", Selection: "6"},
	{Name: "dark", User: "#7dcfff", Dim: "#6b7089", Err: "#f7768e", OK: "#9ece6a", Add: "#9ece6a", Del: "#f7768e", Hunk: "#7aa2f7", Warn: "#e0af68", Status: "#3b4261", Code: "#e0af68", Accent: "#bb9af7", Selection: "#7dcfff"},
	{Name: "light", User: "#0b6e99", Dim: "#6e7781", Err: "#cf222e", OK: "#1a7f37", Add: "#1a7f37", Del: "#cf222e", Hunk: "#0969da", Warn: "#9a6700", Status: "#d0d7de", Code: "#953800", Accent: "#8250df", Selection: "#0969da"},
	{Name: "high-contrast", User: "14", Dim: "7", Err: "9", OK: "10", Add: "10", Del: "9", Hunk: "14", Warn: "11", Status: "15", Code: "11", Accent: "13", Selection: "15"},
	{Name: "solarized", User: "#2aa198", Dim: "#586e75", Err: "#dc322f", OK: "#859900", Add: "#859900", Del: "#dc322f", Hunk: "#268bd2", Warn: "#b58900", Status: "#073642", Code: "#cb4b16", Accent: "#6c71c4", Selection: "#2aa198"},
	{Name: "nord", User: "#88c0d0", Dim: "#616e88", Err: "#bf616a", OK: "#a3be8c", Add: "#a3be8c", Del: "#bf616a", Hunk: "#81a1c1", Warn: "#ebcb8b", Status: "#434c5e", Code: "#d08770", Accent: "#b48ead", Selection: "#88c0d0"},
	{Name: "gruvbox", User: "#8ec07c", Dim: "#928374", Err: "#fb4934", OK: "#b8bb26", Add: "#b8bb26", Del: "#fb4934", Hunk: "#83a598", Warn: "#fabd2f", Status: "#504945", Code: "#fe8019", Accent: "#d3869b", Selection: "#8ec07c"},
}

func init() {
	applyTheme(themes[0])
	register(command{name: "themes", args: "[name]", help: "pick a color theme (live preview)", run: (*Model).cmdThemes})
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

// applyTheme points the package styles at the theme's colors.
func applyTheme(t Theme) {
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
	styleStatus = fg(t.Status).Reverse(true)
	styleToolHdr = fg(t.Accent).Bold(true)
	styleSel = fg(t.Selection).Bold(true)
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
