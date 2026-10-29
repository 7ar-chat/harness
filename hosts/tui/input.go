package tui

import (
	"io"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/GhaziBenDahmane/harness/hosts/client"
)

// This file is the seam between Model and the input/chrome features (mentions,
// shell escape, history, attachments, sidebar, navigation, notifications…):
// model.go calls the few hooks below, each feature keeps its state in inp.

// Attention configures the desktop notifications of a run that needs the user
// while the terminal is not focused. The zero value is disabled.
type Attention struct {
	// Enabled turns on the attention signals (the master switch of /notify).
	Enabled bool
	// Notifications sends an OSC 9 / OSC 777 desktop notification.
	Notifications bool
	// Sound rings the terminal bell.
	Sound bool
}

// inputState is the state of the features that live outside model.go.
type inputState struct {
	workspace string // root of @file and ! (empty: remote mode, both off)
	stateDir  string // history and stash live here ("" keeps them in memory)
	attn      Attention
	out       io.Writer // escape sequences (notifications, clipboard, title) go here
	tipIdx    int

	files      fileIndex
	mention    mentionState
	shellCtx   []string // outputs of !commands the next message carries
	hist       history
	stash      []string
	draft      string // the input before a slash command replaced it
	atts       []attachment
	pastes     map[string]string // "[pasted 12 lines]" -> the full text
	thinking   bool
	timestamps bool
	sb         sidebar
	sel        int // selected block (message navigation), -1 none
	blurred    bool
	title      string // last terminal title set
	cmds       []tea.Cmd
}

func newInputState(o Options) inputState {
	x := inputState{workspace: o.Workspace, stateDir: o.StateDir, attn: o.Attention, out: os.Stdout, sel: -1, pastes: map[string]string{}}
	x.hist = loadHistory(o.StateDir)
	x.stash = loadStash(o.StateDir)
	if o.Now != nil {
		x.tipIdx = int(o.Now().Unix() / 60)
	} else {
		x.tipIdx = int(os.Getpid())
	}
	return x
}

// extraUpdate handles the messages of the features; handled=true ends Update.
func (m *Model) extraUpdate(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case tea.FocusMsg:
		m.inp.blurred = false
		return nil, true
	case tea.BlurMsg:
		m.inp.blurred = true
		return nil, true
	case filesMsg:
		m.inp.files.done(msg)
		return nil, true
	case shellDoneMsg:
		m.shellDone(msg)
		return nil, true
	case editorDoneMsg:
		m.editorDone(msg)
		return nil, true
	case clipImageMsg:
		m.clipImageDone(msg)
		return nil, true
	}
	return nil, false
}

// afterUpdate runs at the end of every Update: it remembers the draft, starts
// the file index, sets the terminal title and drains the commands the
// features queued (notifications).
func (m *Model) afterUpdate() tea.Cmd {
	if v := m.in.Value(); !strings.HasPrefix(v, "/") {
		m.inp.draft = v
	}
	cmds := m.inp.cmds
	m.inp.cmds = nil
	if c := m.indexFiles(); c != nil {
		cmds = append(cmds, c)
	}
	if c := m.setTitle(); c != nil {
		cmds = append(cmds, c)
	}
	return tea.Batch(cmds...)
}

// draftText is the text a command acts on: the input, or (a slash command
// emptied it) what the input held before.
func (m *Model) draftText() string {
	if v := m.in.Value(); v != "" {
		return v
	}
	return m.inp.draft
}

// extraKey handles the keys of the features before the generic ones.
func (m *Model) extraKey(k tea.KeyMsg) (tea.Cmd, bool) {
	s := k.String()
	approval := m.pending != nil && m.pending.prompt.Reason == client.ReasonToolApproval
	switch s {
	case "ctrl+b":
		m.toggleSidebar()
		return nil, true
	case "alt+up", "alt+down":
		if m.in.Value() == "" && !approval {
			m.selectBlock(s == "alt+up")
			return nil, true
		}
	case "alt+y":
		return m.copySelection(), true
	case "esc":
		if m.inp.sel >= 0 {
			m.inp.sel = -1
			return nil, true
		}
	}
	if approval {
		return nil, false
	}
	if k.Paste {
		return m.paste(string(k.Runes))
	}
	if cmd, ok := m.mentionKey(s); ok {
		return cmd, true
	}
	if s == "backspace" && m.in.Value() == "" && len(m.inp.atts) > 0 {
		m.inp.atts = m.inp.atts[:len(m.inp.atts)-1]
		return nil, true
	}
	if s == "up" || s == "down" {
		return nil, m.historyKey(s == "up")
	}
	return nil, false
}

// extraOverlay is what the features show above the status line: the
// attachment chips and the @file menu.
func (m *Model) extraOverlay(w int) string {
	var parts []string
	if c := m.chipsView(w); c != "" {
		parts = append(parts, c)
	}
	if v := m.mentionView(w); v != "" {
		parts = append(parts, v)
	}
	return strings.Join(parts, "\n")
}
