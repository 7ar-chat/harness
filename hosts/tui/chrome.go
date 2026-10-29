package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/GhaziBenDahmane/harness/engine/agui"
)

func init() {
	register(
		command{name: "thinking", help: "show / hide the model's reasoning text", run: func(m *Model, _ string) tea.Cmd {
			m.inp.thinking = !m.inp.thinking
			return nil
		}},
		command{name: "details", help: "expand / collapse tool details (ctrl+o)", run: func(m *Model, _ string) tea.Cmd { m.expanded = !m.expanded; return nil }},
		command{name: "timestamps", help: "show / hide the time of each message", run: func(m *Model, _ string) tea.Cmd {
			m.inp.timestamps = !m.inp.timestamps
			return nil
		}},
		command{name: "copy", help: "copy the selected message (alt+up/down selects), else the last answer", run: func(m *Model, _ string) tea.Cmd { return m.copySelection() }},
		command{name: "notify", help: "toggle desktop notifications when a run needs you and the terminal is in the background", run: (*Model).cmdNotify},
	)
}

// renderBlock renders block i for a transcript w columns wide.
func (m *Model) renderBlock(i, w int) string {
	b := m.blocks[i]
	selected := i == m.inp.sel
	if selected {
		w -= 2
	}
	var s string
	switch b.kind {
	case blockUser:
		s = RenderUser(b.text, w)
	case blockAssistant:
		s = RenderMarkdown(b.text, w)
	case blockTool:
		s = RenderToolCall(b.tool, w, m.expanded)
	case blockReasoning:
		s = m.renderThinking(b, w)
	case blockSteer:
		s = RenderSteer(b.text, b.delivered)
	case blockError:
		s = b.text
		if !strings.HasPrefix(s, "error") {
			s = RenderRunError("", s)
		}
	case blockNotice:
		s = RenderNotice(b.text)
	}
	if m.inp.timestamps && !b.at.IsZero() && b.kind != blockReasoning {
		first, rest, _ := strings.Cut(s, "\n")
		s = first + styleDim.Render("  "+b.at.Format("15:04"))
		if rest != "" {
			s += "\n" + rest
		}
	}
	if selected {
		bar := styleSel.Render("▌ ")
		ls := strings.Split(s, "\n")
		for j := range ls {
			ls[j] = bar + ls[j]
		}
		s = strings.Join(ls, "\n")
	}
	return s
}

// renderThinking draws a reasoning block. Hidden (the default) it is one dim
// line, except while it streams, when the last three lines show; /thinking
// shows all of it.
func (m *Model) renderThinking(b block, w int) string {
	text := strings.TrimSpace(b.tool.Output)
	if text == "" || (!m.inp.thinking && !b.open) {
		return RenderReasoning(len([]rune(b.tool.Output)), b.open)
	}
	ls := strings.Split(ansi.Wrap(text, max(w-2, 10), ""), "\n")
	if !m.inp.thinking && len(ls) > 3 {
		ls = ls[len(ls)-3:]
	}
	head := "✻ thinking…"
	if !b.open {
		head = fmt.Sprintf("✻ thought (%d chars)", len([]rune(b.tool.Output)))
	}
	out := []string{styleDim.Render(head)}
	for _, l := range ls {
		out = append(out, styleDim.Render("  "+l))
	}
	return strings.Join(out, "\n")
}

// content is what the transcript viewport shows: the welcome screen of an
// empty conversation, else the blocks.
func (m *Model) content() string {
	if len(m.blocks) == 0 {
		return m.welcome()
	}
	return m.renderBlocks()
}

// blockOffset is the line of the transcript where block i starts.
func (m *Model) blockOffset(i int) int {
	w, n := m.contentWidth(), 0
	for j := 0; j < i; j++ {
		n += strings.Count(m.renderBlock(j, w), "\n") + 2
	}
	return n
}

// selectBlock moves the selection to the previous (up) or next block and
// scrolls it into view.
func (m *Model) selectBlock(up bool) {
	n := len(m.blocks)
	if n == 0 {
		return
	}
	s := m.inp.sel
	switch {
	case up && s < 0:
		s = n - 1
	case up:
		s = max(s-1, 0)
	case s < 0:
		return
	case s >= n-1:
		s = -1
	default:
		s++
	}
	m.inp.sel = s
	if s < 0 {
		m.vp.GotoBottom()
		return
	}
	m.vp.SetContent(m.renderBlocks())
	m.vp.SetYOffset(max(m.blockOffset(s)-1, 0))
}

// blockText is the plain text of a block, what copying it puts on the clipboard.
func blockText(b block) string {
	switch b.kind {
	case blockTool:
		s := b.tool.Name
		if sum := ToolSummary(b.tool.Args); sum != "" {
			s += " " + sum
		}
		if r := strings.TrimSpace(b.tool.Result); r != "" {
			s += "\n" + r
		}
		return s
	case blockReasoning:
		return b.tool.Output
	}
	return b.text
}

// copySelection copies the selected block, else the last assistant message.
func (m *Model) copySelection() tea.Cmd {
	var text string
	if s := m.inp.sel; s >= 0 && s < len(m.blocks) {
		text = blockText(m.blocks[s])
	} else {
		for i := len(m.blocks) - 1; i >= 0; i-- {
			if m.blocks[i].kind == blockAssistant {
				text = m.blocks[i].text
				break
			}
		}
	}
	if strings.TrimSpace(text) == "" {
		m.add(block{kind: blockNotice, text: "nothing to copy"})
		return nil
	}
	m.add(block{kind: blockNotice, text: fmt.Sprintf("copied %d characters", len([]rune(text)))})
	return m.writeClipboard(text)
}

// writeClipboard sets the system clipboard with OSC 52 (which works over ssh)
// and, when one is installed, pbcopy, wl-copy or xclip.
func (m *Model) writeClipboard(text string) tea.Cmd {
	out, local := m.inp.out, !m.static
	return func() tea.Msg {
		fmt.Fprint(out, ansi.SetSystemClipboard(text))
		if local {
			for _, c := range [][]string{{"pbcopy"}, {"wl-copy"}, {"xclip", "-selection", "clipboard"}} {
				if p, err := exec.LookPath(c[0]); err == nil {
					cmd := exec.Command(p, c[1:]...)
					cmd.Stdin = strings.NewReader(text)
					if cmd.Run() == nil {
						break
					}
				}
			}
		}
		return nil
	}
}

// --- attention notifications and the terminal title ---

func (m *Model) cmdNotify(string) tea.Cmd {
	a := &m.inp.attn
	a.Enabled = !a.Enabled
	state := "off"
	if a.Enabled {
		state = "on"
		if !a.Notifications && !a.Sound {
			a.Notifications, a.Sound = true, true
		}
	}
	m.add(block{kind: blockNotice, text: "notifications: " + state + " (only while this terminal is in the background)"})
	return nil
}

// attention queues a notification for the events that need the user back.
func (m *Model) attention(ev Ev) {
	switch ev.Type {
	case agui.RunError:
		m.notify("harness: run failed", ev.Message)
	case agui.RunFinished:
		switch ev.Outcome {
		case "cancelled":
		case "interrupt":
			if m.pending != nil && m.pending.prompt.Reason == "tool_approval" {
				m.notify("harness: approval needed", m.pending.prompt.Title)
			} else if m.pending != nil {
				m.notify("harness: question", m.pending.prompt.Title)
			}
		default:
			m.notify("harness: run finished", m.sessionTitle())
		}
	}
}

// notify rings the bell and sends a desktop notification, when enabled and the
// terminal is blurred. The text is stripped of control characters.
func (m *Model) notify(title, body string) {
	a := m.inp.attn
	if !a.Enabled || !m.inp.blurred {
		return
	}
	seq := ""
	if a.Sound {
		seq += "\a"
	}
	if a.Notifications {
		seq += notifySeq(termProgram(), cleanText(title), cleanText(body))
	}
	if seq == "" {
		return
	}
	out := m.inp.out
	m.inp.cmds = append(m.inp.cmds, func() tea.Msg { fmt.Fprint(out, seq); return nil })
}

// notifySeq is the escape sequence of a desktop notification: OSC 777 for the
// terminals that read it, OSC 9 for the others.
func notifySeq(term, title, body string) string {
	switch term {
	case "ghostty", "foot", "WezTerm", "rxvt-unicode":
		return "\x1b]777;notify;" + title + ";" + body + "\x07"
	}
	if body != "" {
		title += ": " + body
	}
	return "\x1b]9;" + title + "\x07"
}

func cleanText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ';' {
			return ' '
		}
		return r
	}, s)
	return clip(strings.TrimSpace(s), 120)
}

// titleFor is the terminal title for a session title and run state.
func titleFor(session, state string) string {
	t := "harness"
	if session != "" {
		t += " · " + session
	}
	if state != "" && state != "idle" {
		t += " · " + state
	}
	return t
}

// setTitle updates the terminal title (OSC 2) when the session title or state changed.
func (m *Model) setTitle() tea.Cmd {
	if m.static {
		return nil
	}
	t := titleFor(clip(m.sessionTitle(), 40), m.statusView().State)
	if t == m.inp.title {
		return nil
	}
	m.inp.title = t
	return tea.SetWindowTitle(t)
}

func termProgram() string { return os.Getenv("TERM_PROGRAM") }
