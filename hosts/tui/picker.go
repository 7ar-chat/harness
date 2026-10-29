package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var styleSel = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)

// pickRows is how many items a picker shows at once.
const pickRows = 8

type pickItem struct {
	id      string
	label   string
	desc    string
	current bool
}

// picker is a filterable list shown above the input: type to narrow, up/down
// to move, enter to choose, esc to leave.
type picker struct {
	title  string
	items  []pickItem
	filter string
	sel    int
	// custom lets enter choose the typed filter when nothing matches (a model id the list lacks).
	custom bool
	pick   func(m *Model, it pickItem) tea.Cmd
}

func (p *picker) visible() []pickItem {
	f := strings.ToLower(p.filter)
	if f == "" {
		return p.items
	}
	var out []pickItem
	for _, it := range p.items {
		if strings.Contains(strings.ToLower(it.id+" "+it.label+" "+it.desc), f) {
			out = append(out, it)
		}
	}
	return out
}

// key handles a key; done reports that the picker should close, and chosen
// (when non-nil) is the item picked.
func (p *picker) key(k tea.KeyMsg) (done bool, chosen *pickItem) {
	vis := p.visible()
	switch k.String() {
	case "esc", "ctrl+c":
		return true, nil
	case "up", "ctrl+p":
		if p.sel > 0 {
			p.sel--
		}
	case "down", "ctrl+n":
		if p.sel < len(vis)-1 {
			p.sel++
		}
	case "enter":
		if p.sel < len(vis) {
			it := vis[p.sel]
			return true, &it
		}
		if p.custom && strings.TrimSpace(p.filter) != "" {
			id := strings.TrimSpace(p.filter)
			return true, &pickItem{id: id, label: id}
		}
	case "backspace":
		if r := []rune(p.filter); len(r) > 0 {
			p.filter = string(r[:len(r)-1])
			p.sel = 0
		}
	default:
		if k.Type == tea.KeyRunes || k.Type == tea.KeySpace {
			p.filter += string(k.Runes)
			if k.Type == tea.KeySpace {
				p.filter += " "
			}
			p.sel = 0
		}
	}
	return false, nil
}

func (p *picker) view(width int) string {
	vis := p.visible()
	lines := []string{styleBold.Render(p.title) + styleDim.Render("  type to filter · ↑↓ · enter · esc")}
	lines = append(lines, styleDim.Render("> ")+p.filter)
	start := 0
	if p.sel >= pickRows {
		start = p.sel - pickRows + 1
	}
	for i := start; i < len(vis) && i < start+pickRows; i++ {
		it := vis[i]
		mark := "  "
		if it.current {
			mark = "● "
		}
		line := mark + it.label
		if it.desc != "" {
			line += "  " + it.desc
		}
		line = clip(line, width-2)
		if i == p.sel {
			line = styleSel.Render("▸ " + line)
		} else {
			line = "  " + line
			if it.desc != "" {
				line = styleDim.Render(line)
			}
		}
		lines = append(lines, line)
	}
	switch {
	case len(vis) == 0 && p.custom && strings.TrimSpace(p.filter) != "":
		lines = append(lines, styleDim.Render(fmt.Sprintf("  no match — enter uses %q as is", strings.TrimSpace(p.filter))))
	case len(vis) == 0:
		lines = append(lines, styleDim.Render("  no match"))
	case len(vis) > pickRows:
		lines = append(lines, styleDim.Render(fmt.Sprintf("  %d/%d", p.sel+1, len(vis))))
	}
	return strings.Join(lines, "\n")
}
