package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// pickRows is how many items a picker shows at once.
const pickRows = 8

type pickItem struct {
	id      string
	label   string
	desc    string
	current bool
	// group heads a run of items in the list ("" for none); the view prints it when it changes.
	group string
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
	// preview (optional) is called with the highlighted item whenever the selection moves; cancel when esc leaves the picker.
	preview func(it pickItem)
	cancel  func()
	// extra sees every key first; ok=true means it consumed the key (rename, delete, favorite, key entry).
	extra func(m *Model, k tea.KeyMsg) (cmd tea.Cmd, ok bool)
	// hint replaces the default key hint of the title line; footer is an extra line below the list (a prompt or a confirmation).
	hint, footer string
	// rows is how many items show at once (0: pickRows).
	rows int
	// dimDesc dims only the description of a row, not its label.
	dimDesc bool
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
		if p.cancel != nil {
			p.cancel()
		}
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
	p.notify()
	return false, nil
}

// notify previews the highlighted item (the picker is closed when preview is nil).
func (p *picker) notify() {
	if vis := p.visible(); p.preview != nil && p.sel < len(vis) {
		p.preview(vis[p.sel])
	}
}

func (p *picker) view(width int) string {
	vis := p.visible()
	rows := pickRows
	if p.rows > 0 {
		rows = p.rows
	}
	hint := "type to filter · ↑↓ · enter · esc"
	if p.hint != "" {
		hint = p.hint
	}
	lines := []string{styleBold.Render(p.title) + styleDim.Render("  "+hint)}
	lines = append(lines, styleDim.Render("> ")+p.filter)
	start := 0
	if p.sel >= rows {
		start = p.sel - rows + 1
	}
	for i := start; i < len(vis) && i < start+rows; i++ {
		it := vis[i]
		if it.group != "" && (i == start || vis[i-1].group != it.group) {
			lines = append(lines, styleDim.Render("  ── "+it.group))
		}
		mark := "  "
		if it.current {
			mark = "● "
		}
		head := mark + it.label
		line := head
		if it.desc != "" {
			line += "  " + it.desc
		}
		line = clip(line, width-2)
		if i == p.sel {
			line = styleSel.Render("▸ " + line)
		} else if r := []rune(line); p.dimDesc && it.desc != "" && len(r) > len([]rune(head)) {
			line = "  " + string(r[:len([]rune(head))]) + styleDim.Render(string(r[len([]rune(head)):]))
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
	case len(vis) > rows:
		lines = append(lines, styleDim.Render(fmt.Sprintf("  %d/%d", p.sel+1, len(vis))))
	}
	if p.footer != "" {
		lines = append(lines, p.footer)
	}
	return strings.Join(lines, "\n")
}
