package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// A tip names a command of the registry or an action of the keymap, so the key
// or the name shown cannot go stale (a test checks that each one exists), or
// is plain text about a key that is not in either (the input, the lists).
type tip struct {
	cmd    string // a slash command: the tip reads "/cmd [leader key] text"
	action string // a keymap action: the tip reads "<its key> text"
	text   string
}

// tipPool rotates in the welcome screen, one per launch (/tips hides it).
var tipPool = []tip{
	{action: "tools", text: "expands the tool calls of the transcript"},
	{cmd: "model", text: "switches the model; f2 and shift+f2 cycle the recent ones"},
	{cmd: "sessions", text: "resumes an earlier conversation; ctrl+o in the list pins one, ctrl+r renames it"},
	{text: "alt+up and alt+down select a message, alt+y copies it"},
	{cmd: "stash", text: "parks the half-written message; /stash pop brings it back"},
	{cmd: "editor", text: "composes a long message in $EDITOR"},
	{cmd: "thinking", text: "shows the model's reasoning; /timestamps shows the time of each message"},
	{text: "ctrl+b opens the side panel with the plan and the files the agent changed"},
	{text: "paste or drop a png/jpg path to attach an image; /image <path> does too"},
	{cmd: "export", text: "writes the conversation as Markdown"},
	{cmd: "notify", text: "rings the terminal when a run needs you and the window is in the background"},
	{text: "@ picks a workspace file to send with your message; a message starting with ! runs a shell command"},
	{cmd: "timeline", text: "jumps to one of your earlier messages"},
	{cmd: "diff", text: "switches the diffs of edits between side by side (wide terminals) and stacked"},
	{cmd: "undo", text: "takes back the last prompt and restores the files; /redo brings it back"},
	{action: "palette", text: "searches every command"},
	{cmd: "plan", text: "makes the agent read-only; tab toggles between plan and build"},
	{cmd: "effort", text: "sets the reasoning effort of the next runs; ctrl+t cycles it"},
	{cmd: "themes", text: "previews the color themes live and keeps your choice"},
	{text: "up and down walk the history of what you sent"},
	{cmd: "status", text: "shows the backend, model, workspace and configuration files in effect"},
	{cmd: "compact", text: "summarises the conversation to free context; /fork continues in a copy"},
}

func init() {
	register(command{name: "tips", bind: "h", help: "show / hide the tip of the welcome screen", run: (*Model).cmdTips})
}

// render writes the tip with the keys the user's keymap gives.
func (t tip) render(m *Model) string {
	switch {
	case t.cmd != "":
		s := "/" + t.cmd
		if c, ok := lookup(t.cmd); ok {
			if k := m.keys.keyLabel(c); k != "" {
				s += " [" + k + "]"
			}
		}
		return s + " " + t.text
	case t.action != "":
		return strings.Join(m.keys.keys[t.action], "/") + " " + t.text
	}
	return t.text
}

// tipLine is the "tip: …" line of the welcome screen, "" when tips are hidden.
func (m *Model) tipLine() string {
	if m.pf.tipsOff {
		return ""
	}
	return "tip: " + tipPool[m.inp.tipIdx%len(tipPool)].render(m)
}

func (m *Model) cmdTips(string) tea.Cmd {
	m.pf.tipsOff = !m.pf.tipsOff
	state := "shown"
	if m.pf.tipsOff {
		state = "hidden"
	}
	if err := m.uiStore.set(stateTipsOff, m.pf.tipsOff); err != nil {
		m.notice("tips: could not save the choice: %v", err)
	}
	m.notice("tips: %s", state)
	return nil
}
