package tui

import (
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// The model picker's favorites (ctrl+f), recents (the last five used, f2 and
// shift+f2 cycle them) and the grouping of "provider/model" ids. Favorites and
// recents live in tui-state.json under the keys "favorites" and "recents".

const maxRecents = 5

// useModel switches to id and remembers it as the most recent.
func (m *Model) useModel(mb ModelBackend, id string) {
	mb.SetModel(id)
	m.ext.recents = append([]string{id}, slices.DeleteFunc(slices.Clone(m.ext.recents), func(r string) bool { return r == id })...)
	if len(m.ext.recents) > maxRecents {
		m.ext.recents = m.ext.recents[:maxRecents]
	}
	m.saveState("recents", m.ext.recents)
	m.notice("model: %s", id)
}

// saveState persists one key of the state file; a failure is a notice, never fatal.
func (m *Model) saveState(key string, v any) {
	if err := m.ext.state.set(key, v); err != nil {
		m.notice("could not save %s: %v", key, err)
	}
}

func (m *Model) toggleFavorite(id string) {
	if i := slices.Index(m.ext.favorites, id); i >= 0 {
		m.ext.favorites = slices.Delete(m.ext.favorites, i, i+1)
	} else {
		m.ext.favorites = append(m.ext.favorites, id)
	}
	m.saveState("favorites", m.ext.favorites)
}

// cycleRecent switches to the next (dir 1) or previous (dir -1) recent model.
func (m *Model) cycleRecent(dir int) {
	mb, ok := m.be.(ModelBackend)
	if !ok {
		m.notice("this backend cannot switch models")
		return
	}
	rec := m.ext.recents
	cur := slices.Index(rec, m.be.Model())
	if len(rec) == 0 || (len(rec) == 1 && cur == 0) {
		m.notice("no other recent model — /model picks one")
		return
	}
	next := 0
	switch {
	case cur >= 0:
		next = (cur + dir + len(rec)) % len(rec)
	case dir < 0:
		next = len(rec) - 1
	}
	mb.SetModel(rec[next])
	m.notice("model: %s (recent %d/%d)", rec[next], next+1, len(rec))
}

// modelItems orders the picker: favorites, recents, then the rest grouped by
// provider when ids look like "provider/model".
func (m *Model) modelItems(ids []string, cur string) []pickItem {
	all := slices.Clone(ids)
	if cur != "" && !slices.Contains(all, cur) {
		all = append([]string{cur}, all...)
	}
	known := func(id string) bool { return slices.Contains(all, id) }
	var favs, recs, rest []string
	for _, id := range m.ext.favorites {
		if known(id) {
			favs = append(favs, id)
		}
	}
	for _, id := range m.ext.recents {
		if known(id) && !slices.Contains(favs, id) {
			recs = append(recs, id)
		}
	}
	slash := false
	for _, id := range all {
		if !slices.Contains(favs, id) && !slices.Contains(recs, id) {
			rest = append(rest, id)
		}
		slash = slash || strings.Contains(id, "/")
	}
	grouped := slash || len(favs) > 0 || len(recs) > 0
	provider := func(id string) string {
		if p, _, ok := strings.Cut(id, "/"); ok && p != "" {
			return p
		}
		if slash {
			return "other"
		}
		return "models"
	}
	var items []pickItem
	add := func(id, group string) {
		label := id
		if slices.Contains(m.ext.favorites, id) {
			label = "★ " + id
		}
		if !grouped {
			group = ""
		}
		items = append(items, pickItem{id: id, label: label, current: id == cur, group: group})
	}
	for _, id := range favs {
		add(id, "favorites")
	}
	for _, id := range recs {
		add(id, "recent")
	}
	// Providers in the order they first appear, models in the order given.
	var order []string
	for _, id := range rest {
		if p := provider(id); !slices.Contains(order, p) {
			order = append(order, p)
		}
	}
	for _, p := range order {
		for _, id := range rest {
			if provider(id) == p {
				add(id, p)
			}
		}
	}
	return items
}

func (m *Model) openModelPickerX(ids []string) {
	cur := m.be.Model()
	p := &picker{title: "Model", custom: true, hint: "type to filter · ↑↓ · enter · ctrl+f favorite · esc",
		pick: func(m *Model, it pickItem) tea.Cmd {
			m.useModel(m.be.(ModelBackend), it.id)
			return nil
		}}
	p.items = m.modelItems(ids, cur)
	for i, it := range p.items {
		if it.current {
			p.sel = i
		}
	}
	p.extra = func(m *Model, k tea.KeyMsg) (tea.Cmd, bool) {
		if k.String() != "ctrl+f" {
			return nil, false
		}
		if vis := p.visible(); p.sel < len(vis) {
			id := vis[p.sel].id
			m.toggleFavorite(id)
			p.items = m.modelItems(ids, cur)
			p.sel = 0
			for i, it := range p.visible() { // keep the cursor on the same model
				if it.id == id {
					p.sel = i
				}
			}
		}
		return nil, true
	}
	m.picker = p
}
