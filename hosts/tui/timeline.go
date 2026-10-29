package tui

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// The message timeline: a picker over what the user said in this conversation
// that scrolls the transcript to the chosen message, and the keymap actions
// messages_next / messages_previous / messages_last_user that do the same
// without the picker.

func init() {
	register(command{name: "timeline", bind: "g", help: "jump to one of your messages in this conversation", run: (*Model).cmdTimeline})
}

// blockOffsets are the transcript lines where each block starts, from one
// rendering pass (blockOffset renders the blocks before one block every time).
func (m *Model) blockOffsets() []int {
	w := m.contentWidth()
	offs := make([]int, len(m.blocks))
	n := 0
	for i := range m.blocks {
		offs[i] = n
		n += strings.Count(m.renderBlock(i, w), "\n") + 2
	}
	return offs
}

// userBlocks are the indexes of the messages the user sent.
func (m *Model) userBlocks() []int {
	var out []int
	for i, b := range m.blocks {
		if b.kind == blockUser {
			out = append(out, i)
		}
	}
	return out
}

// showBlock selects block i and scrolls the transcript so it starts the view.
func (m *Model) showBlock(i int) {
	m.inp.sel = i
	m.vp.SetContent(m.renderBlocks())
	m.vp.SetYOffset(m.blockOffsets()[i])
}

// jumpUser scrolls to the next (dir 1) or previous (dir -1) message of the
// user relative to the top of the view, or to the last one (dir 0).
func (m *Model) jumpUser(dir int) {
	users := m.userBlocks()
	if len(users) == 0 {
		m.notice("no messages of yours in this conversation yet")
		return
	}
	offs := m.blockOffsets()
	top := m.vp.YOffset
	target := users[len(users)-1]
	switch dir {
	case 1:
		target = -1
		for _, u := range users {
			if offs[u] > top {
				target = u
				break
			}
		}
		if target < 0 {
			m.vp.GotoBottom()
			m.inp.sel = -1
			return
		}
	case -1:
		target = users[0]
		for _, u := range users {
			if offs[u] < top {
				target = u
			}
		}
	}
	m.showBlock(target)
}

func (m *Model) cmdTimeline(string) tea.Cmd {
	users := m.userBlocks()
	if len(users) == 0 {
		m.notice("no messages of yours in this conversation yet")
		return nil
	}
	items := make([]pickItem, len(users))
	for k, i := range users {
		b := m.blocks[i]
		first, _, _ := strings.Cut(strings.TrimSpace(b.text), "\n")
		desc := ""
		if !b.at.IsZero() {
			desc = b.at.Format("Jan 2 15:04")
		}
		items[k] = pickItem{id: strconv.Itoa(i), label: clip(first, 70), desc: desc}
	}
	m.picker = &picker{title: "Timeline", hint: "↑↓ · enter jumps · type to filter · esc", items: items, sel: len(items) - 1,
		pick: func(m *Model, it pickItem) tea.Cmd {
			if i, err := strconv.Atoi(it.id); err == nil && i >= 0 && i < len(m.blocks) {
				m.showBlock(i)
			}
			return nil
		}}
	return nil
}
