package tui

import (
	"context"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/7ar-chat/harness/hosts/client"
)

// /undo, /redo, /compact and /fork: edits of the conversation. Each refuses
// while a run is active and degrades to a notice on a backend or server
// without the capability.

func init() {
	register(
		command{name: "undo", bind: "u", help: "remove the last prompt and its answers, restore the files", run: (*Model).cmdUndo},
		command{name: "redo", bind: "r", help: "reapply what /undo removed", run: (*Model).cmdRedo},
		command{name: "compact", aliases: []string{"summarize"}, bind: "c", help: "summarise the conversation now", run: (*Model).cmdCompact},
		command{name: "fork", bind: "f", help: "continue in a copy of this conversation", run: (*Model).cmdFork},
	)
}

type (
	// undoMsg: an Undo or Redo finished; th is the reloaded thread (nil when it could not be).
	undoMsg struct {
		op  string
		res *client.UndoResult
		th  *client.Thread
		err error
	}
	// compactMsg: Compact finished.
	compactMsg struct {
		res *client.CompactResult
		err error
	}
	// forkMsg: Fork finished; th is the copy.
	forkMsg struct {
		th  *client.Thread
		err error
	}
)

// history returns the edit capability, or tells the user it is missing (and that a run in progress blocks it).
func (m *Model) history(op string) (HistoryBackend, bool) {
	if m.busy(op) {
		return nil, false
	}
	hb, ok := m.be.(HistoryBackend)
	if !ok {
		m.notice("%s: not supported by this backend/server", op)
	}
	return hb, ok
}

// reload fetches the current thread again; nil when the backend cannot say which it is.
func (m *Model) reload() *client.Thread {
	tb, ok1 := m.be.(ThreadAdminBackend)
	sb, ok2 := m.be.(SessionBackend)
	if !ok1 || !ok2 {
		return nil
	}
	th, err := sb.OpenThread(m.ctx, tb.CurrentThread())
	if err != nil {
		return nil
	}
	return th
}

func (m *Model) cmdUndo(string) tea.Cmd {
	return m.undoStep("/undo", func(h HistoryBackend, ctx context.Context) (*client.UndoResult, error) { return h.Undo(ctx) })
}

func (m *Model) cmdRedo(string) tea.Cmd {
	return m.undoStep("/redo", func(h HistoryBackend, ctx context.Context) (*client.UndoResult, error) { return h.Redo(ctx) })
}

func (m *Model) undoStep(op string, step func(HistoryBackend, context.Context) (*client.UndoResult, error)) tea.Cmd {
	hb, ok := m.history(op)
	if !ok {
		return nil
	}
	return func() tea.Msg {
		res, err := step(hb, m.ctx)
		if err != nil {
			return undoMsg{op: op, err: err}
		}
		if res == nil {
			res = &client.UndoResult{}
		}
		return undoMsg{op: op, res: res, th: m.reload()}
	}
}

func (u undoMsg) apply(m *Model) tea.Cmd {
	if u.err != nil {
		if !m.unsupported(u.op, u.err) {
			m.notice("%s: %v", u.op, u.err)
		}
		return nil
	}
	if u.th != nil {
		m.dropReloaded(u.th)
	}
	verb := "undone"
	if u.op == "/redo" {
		verb = "redone"
	} else if u.res.Prompt != "" {
		m.in.SetValue(u.res.Prompt) // the removed prompt, ready to edit and send again
		m.in.CursorEnd()
	}
	text := verb
	switch n := len(u.res.Files); {
	case n == 0:
		text += " (no file changes)"
	default:
		shown := u.res.Files
		if n > 5 {
			shown = shown[:5]
		}
		text += ": " + plural(n, "file") + " changed — " + strings.Join(shown, ", ")
		if n > 5 {
			text += ", …"
		}
	}
	if u.res.CanUndo || u.res.CanRedo {
		var more []string
		if u.res.CanUndo {
			more = append(more, "/undo")
		}
		if u.res.CanRedo {
			more = append(more, "/redo")
		}
		text += " · " + strings.Join(more, " and ") + " available"
	}
	m.notice("%s", text)
	return nil
}

// dropReloaded replays th without the "resumed" notice loadThread appends.
func (m *Model) dropReloaded(th *client.Thread) {
	m.loadThread(th)
	m.blocks = m.blocks[:len(m.blocks)-1]
	m.vp.GotoBottom()
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

func (m *Model) cmdCompact(string) tea.Cmd {
	hb, ok := m.history("/compact")
	if !ok {
		return nil
	}
	m.notice("compacting…")
	return func() tea.Msg {
		res, err := hb.Compact(m.ctx)
		return compactMsg{res, err}
	}
}

func (c compactMsg) apply(m *Model) tea.Cmd {
	switch {
	case c.err != nil:
		if !m.unsupported("/compact", c.err) {
			m.notice("/compact: %v", c.err)
		}
	case !c.res.Compacted:
		m.notice("nothing to compact")
	case c.res.TokensBefore > 0:
		m.notice("compacted: %s → %s tokens", FormatTokens(c.res.TokensBefore), FormatTokens(c.res.TokensAfter))
	default:
		m.notice("context compacted")
	}
	return nil
}

func (m *Model) cmdFork(string) tea.Cmd {
	hb, ok := m.history("/fork")
	if !ok {
		return nil
	}
	return func() tea.Msg {
		th, err := hb.Fork(m.ctx, "")
		if err != nil {
			return forkMsg{err: err}
		}
		if sb, ok := m.be.(SessionBackend); ok { // the copy with its messages
			if full, err := sb.OpenThread(m.ctx, th.ID); err == nil {
				th = full
			}
		}
		return forkMsg{th: th}
	}
}

func (f forkMsg) apply(m *Model) tea.Cmd {
	if f.err != nil {
		if !m.unsupported("/fork", f.err) {
			m.notice("/fork: %v", f.err)
		}
		return nil
	}
	m.dropReloaded(f.th)
	title := f.th.Title
	if title == "" {
		title = "(untitled)"
	}
	m.notice("forked: %s — the original is unchanged", title)
	return nil
}
