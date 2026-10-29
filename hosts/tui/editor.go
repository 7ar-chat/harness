package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// editorDoneMsg: the external editor of /editor or /export exited.
type editorDoneMsg struct {
	path   string
	err    error
	export bool // the file was an export: leave it, do not load it into the input
}

func init() {
	register(
		command{name: "editor", bind: "e", help: "compose the message in $EDITOR", run: (*Model).cmdEditor},
		command{name: "export", bind: "x", help: "write the conversation as Markdown and open it in $EDITOR", run: (*Model).cmdExport},
	)
}

// editorCommand splits $VISUAL or $EDITOR into program and arguments, so
// "code --wait" works. It returns nil when neither is set.
func editorCommand() []string {
	for _, v := range []string{"VISUAL", "EDITOR"} {
		if f := strings.Fields(os.Getenv(v)); len(f) > 0 {
			return f
		}
	}
	return nil
}

// openEditor suspends the UI and runs the editor on path.
func openEditor(path string, export bool) tea.Cmd {
	ed := editorCommand()
	if ed == nil {
		return func() tea.Msg {
			return editorDoneMsg{path: path, err: fmt.Errorf("set $EDITOR (or $VISUAL) first"), export: export}
		}
	}
	c := exec.Command(ed[0], append(ed[1:], path)...)
	return tea.ExecProcess(c, func(err error) tea.Msg { return editorDoneMsg{path: path, err: err, export: export} })
}

func (m *Model) cmdEditor(string) tea.Cmd {
	f, err := os.CreateTemp("", "har-message-*.md")
	if err != nil {
		m.add(block{kind: blockNotice, text: "editor: " + err.Error()})
		return nil
	}
	_, err = f.WriteString(m.draftText())
	f.Close()
	if err != nil {
		m.add(block{kind: blockNotice, text: "editor: " + err.Error()})
		return nil
	}
	return openEditor(f.Name(), false)
}

func (m *Model) cmdExport(string) tea.Cmd {
	f, err := os.CreateTemp("", "har-export-*.md")
	if err != nil {
		m.add(block{kind: blockNotice, text: "export: " + err.Error()})
		return nil
	}
	_, err = f.WriteString(exportMarkdown(m.sessionTitle(), m.blocks, m.now()))
	f.Close()
	if err != nil {
		m.add(block{kind: blockNotice, text: "export: " + err.Error()})
		return nil
	}
	m.add(block{kind: blockNotice, text: "exported to " + f.Name()})
	if editorCommand() == nil {
		return nil
	}
	return openEditor(f.Name(), true)
}

func (m *Model) editorDone(r editorDoneMsg) {
	if r.export {
		if r.err != nil {
			m.add(block{kind: blockNotice, text: "editor: " + r.err.Error()})
		}
		return
	}
	defer os.Remove(r.path)
	if r.err != nil {
		m.add(block{kind: blockNotice, text: "editor: " + r.err.Error()})
		return
	}
	b, err := os.ReadFile(r.path)
	if err != nil {
		m.add(block{kind: blockNotice, text: "editor: " + err.Error()})
		return
	}
	m.in.SetValue(strings.TrimRight(string(b), "\r\n"))
	m.in.CursorEnd()
}

// Limits of a tool result in an export.
const (
	exportResultLines = 20
	exportResultChars = 2000
)

// exportMarkdown renders the conversation as Markdown: the title, the user and
// assistant text, and each tool call with its (truncated) result.
func exportMarkdown(title string, blocks []block, at time.Time) string {
	var b strings.Builder
	if title == "" {
		title = "Conversation"
	}
	fmt.Fprintf(&b, "# %s\n\n_Exported %s_\n", title, at.Format("2006-01-02 15:04"))
	for _, bl := range blocks {
		switch bl.kind {
		case blockUser, blockSteer:
			fmt.Fprintf(&b, "\n## User\n\n%s\n", strings.TrimSpace(bl.text))
		case blockAssistant:
			fmt.Fprintf(&b, "\n## Assistant\n\n%s\n", strings.TrimSpace(bl.text))
		case blockTool:
			fmt.Fprintf(&b, "\n### Tool: %s\n", bl.tool.Name)
			if s := ToolSummary(bl.tool.Args); s != "" {
				fmt.Fprintf(&b, "\n`%s`\n", strings.ReplaceAll(s, "`", "'"))
			}
			if r := strings.TrimSpace(bl.tool.Result); r != "" {
				fmt.Fprintf(&b, "\n```\n%s\n```\n", truncateResult(r))
			}
		case blockError:
			fmt.Fprintf(&b, "\n> **Error:** %s\n", oneLine(bl.text))
		}
	}
	return b.String()
}

// truncateResult cuts a tool result to exportResultLines lines and
// exportResultChars characters, saying how much was left out.
func truncateResult(s string) string {
	ls := strings.Split(s, "\n")
	cut := 0
	if len(ls) > exportResultLines {
		cut = len(ls) - exportResultLines
		ls = ls[:exportResultLines]
	}
	out := strings.Join(ls, "\n")
	if r := []rune(out); len(r) > exportResultChars {
		out = string(r[:exportResultChars])
		cut = max(cut, 1)
	}
	if cut > 0 {
		out += "\n… (truncated)"
	}
	return strings.ReplaceAll(out, "```", "'''")
}
