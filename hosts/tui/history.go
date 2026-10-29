package tui

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// maxHistory is how many sent messages the input history (and the stash) keep.
const maxHistory = 200

// history is the list of sent messages, oldest first, walked with up/down.
type history struct {
	items []string
	pos   int    // index being shown; -1 when not browsing
	draft string // the input before browsing started
}

// loadHistory reads stateDir/tui-history (one JSON string per line).
func loadHistory(dir string) history {
	return history{items: readLines(dir, "tui-history"), pos: -1}
}

func loadStash(dir string) []string { return readLines(dir, "tui-stash") }

func readLines(dir, name string) []string {
	if dir == "" {
		return nil
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var s string
		if json.Unmarshal(sc.Bytes(), &s) == nil && s != "" {
			out = append(out, s)
		}
	}
	if len(out) > maxHistory {
		out = out[len(out)-maxHistory:]
	}
	return out
}

// writeLines rewrites stateDir/name; failures are ignored (history is a convenience).
func writeLines(dir, name string, items []string) {
	if dir == "" {
		return
	}
	var b strings.Builder
	for _, s := range items {
		line, _ := json.Marshal(s)
		b.Write(line)
		b.WriteByte('\n')
	}
	if os.MkdirAll(dir, 0o700) == nil {
		_ = os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0o600)
	}
}

// add records a sent message (a repeat of the last one is not added) and saves.
func (h *history) add(text, dir string) {
	h.pos, h.draft = -1, ""
	if strings.TrimSpace(text) == "" || (len(h.items) > 0 && h.items[len(h.items)-1] == text) {
		return
	}
	h.items = append(h.items, text)
	if len(h.items) > maxHistory {
		h.items = h.items[len(h.items)-maxHistory:]
	}
	writeLines(dir, "tui-history", h.items)
}

// historyKey walks the history with up/down when the input is empty or already
// showing a history entry and has one line. It reports whether it used the key.
func (m *Model) historyKey(up bool) bool {
	h := &m.ext.hist
	v := m.in.Value()
	browsing := h.pos >= 0 && h.pos < len(h.items) && v == h.items[h.pos]
	if strings.Contains(v, "\n") || (v != "" && !browsing) || m.picker != nil {
		return false
	}
	if !browsing {
		h.pos = -1
	}
	switch {
	case up && len(h.items) == 0:
		return false
	case up && h.pos < 0:
		h.draft, h.pos = v, len(h.items)-1
	case up:
		h.pos = max(h.pos-1, 0)
	case h.pos < 0:
		return false
	case h.pos >= len(h.items)-1:
		h.pos = -1
		m.in.SetValue(h.draft)
		m.in.CursorEnd()
		return true
	default:
		h.pos++
	}
	m.in.SetValue(h.items[h.pos])
	m.in.CursorEnd()
	return true
}

func init() {
	register(command{name: "stash", args: "[pop|list|text]", bind: "s", help: "park the input for later (pop restores the last, list picks one)", run: (*Model).cmdStash})
}

func (m *Model) cmdStash(arg string) tea.Cmd {
	switch arg {
	case "pop":
		if len(m.ext.stash) == 0 {
			m.add(block{kind: blockNotice, text: "the stash is empty"})
			return nil
		}
		m.restoreStash(len(m.ext.stash) - 1)
	case "list":
		if len(m.ext.stash) == 0 {
			m.add(block{kind: blockNotice, text: "the stash is empty"})
			return nil
		}
		items := make([]pickItem, len(m.ext.stash))
		for i := range m.ext.stash {
			j := len(m.ext.stash) - 1 - i // newest first
			items[i] = pickItem{id: strconv.Itoa(j), label: clip(oneLine(m.ext.stash[j]), 70)}
		}
		m.picker = &picker{title: "Stash", items: items, pick: func(m *Model, it pickItem) tea.Cmd {
			if j, _ := strconv.Atoi(it.id); j < len(m.ext.stash) {
				m.restoreStash(j)
			}
			return nil
		}}
	default:
		text := arg
		if text == "" {
			text = m.draftText()
		}
		if strings.TrimSpace(text) == "" {
			m.add(block{kind: blockNotice, text: "nothing to stash: type a message first, or /stash <text>"})
			return nil
		}
		m.ext.stash = append(m.ext.stash, text)
		if len(m.ext.stash) > maxHistory {
			m.ext.stash = m.ext.stash[1:]
		}
		writeLines(m.ext.stateDir, "tui-stash", m.ext.stash)
		m.in.Reset()
		m.ext.draft = ""
		m.add(block{kind: blockNotice, text: "stashed (/stash pop restores it)"})
	}
	return nil
}

// restoreStash puts entry i back in the input and drops it from the stash.
func (m *Model) restoreStash(i int) {
	m.in.SetValue(m.ext.stash[i])
	m.in.CursorEnd()
	m.ext.stash = append(m.ext.stash[:i:i], m.ext.stash[i+1:]...)
	writeLines(m.ext.stateDir, "tui-stash", m.ext.stash)
}
