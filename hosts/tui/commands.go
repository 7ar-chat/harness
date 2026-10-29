package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/GhaziBenDahmane/harness/hosts/client"
	"github.com/GhaziBenDahmane/harness/store"
)

func init() {
	register(
		command{name: "model", aliases: []string{"models"}, args: "[id]", bind: "m", help: "pick a model (discovered from the provider), or set one by id", run: (*Model).cmdModel},
		command{name: "sessions", aliases: []string{"resume", "continue"}, bind: "l", help: "switch to a saved conversation", run: (*Model).cmdSessions},
		command{name: "new", aliases: []string{"clear"}, bind: "n", help: "start a new conversation", run: (*Model).cmdNew},
	)
}

type (
	// modelsMsg is the answer of the model listing started by /model.
	modelsMsg struct {
		ids []string
		err error
	}
	// sessionsMsg is the answer of the session listing started by /sessions.
	sessionsMsg struct {
		list []client.Session
		err  error
	}
	// threadMsg: /new or a session switch finished (th is nil for /new).
	threadMsg struct {
		th  *client.Thread
		err error
	}
)

// busy refuses a command that would replace the conversation under a run.
func (m *Model) busy(what string) bool {
	if m.running || m.starting || m.pending != nil {
		m.add(block{kind: blockNotice, text: what + ": a run is active or waiting for an answer — esc cancels it first"})
		return true
	}
	return false
}

func (m *Model) cmdModel(arg string) tea.Cmd {
	mb, ok := m.be.(ModelBackend)
	if !ok {
		m.add(block{kind: blockNotice, text: "this backend cannot switch models"})
		return nil
	}
	if arg != "" {
		m.useModel(mb, arg)
		return nil
	}
	m.add(block{kind: blockNotice, text: "asking the provider for its models…"})
	return func() tea.Msg {
		ids, err := mb.Models(m.ctx)
		return modelsMsg{ids, err}
	}
}

func (m *Model) openModelPicker(ids []string) { m.openModelPickerX(ids) }

func (m *Model) cmdSessions(string) tea.Cmd {
	sb, ok := m.be.(SessionBackend)
	if !ok {
		m.add(block{kind: blockNotice, text: "this backend has no saved conversations"})
		return nil
	}
	if m.busy("/sessions") {
		return nil
	}
	return func() tea.Msg {
		list, err := sb.Sessions(m.ctx)
		return sessionsMsg{list, err}
	}
}

func (m *Model) openSessionPicker(list []client.Session) { m.openSessionPickerX(list) }

func (m *Model) cmdNew(string) tea.Cmd {
	if m.busy("/new") {
		return nil
	}
	sb, ok := m.be.(SessionBackend)
	if !ok {
		m.resetTranscript()
		return nil
	}
	return func() tea.Msg { return threadMsg{err: sb.NewThread(m.ctx)} }
}

// resetTranscript empties the conversation view (not the session usage of the backend).
func (m *Model) resetTranscript() {
	m.blocks, m.toolIdx = nil, map[string]int{}
	m.runID, m.steps, m.elapsed = "", 0, 0
	m.base, m.cur = Usage{}, Usage{}
	m.pending, m.quitArmed = nil, false
	m.in.Reset()
	m.vp.GotoTop()
}

// loadThread shows a saved conversation: user and assistant text, and the tool calls with their results.
func (m *Model) loadThread(th *client.Thread) {
	m.resetTranscript()
	if th == nil {
		return
	}
	for _, msg := range th.Messages {
		switch msg.Role {
		case store.RoleUser:
			m.add(block{kind: blockUser, text: msg.Content})
		case store.RoleAssistant:
			if msg.Content != "" {
				m.add(block{kind: blockAssistant, text: msg.Content})
			}
			for _, tc := range msg.ToolCalls {
				m.toolIdx[tc.ID] = len(m.blocks)
				m.add(block{kind: blockTool, id: tc.ID, tool: ToolView{Name: tc.Function.Name, Args: tc.Function.Arguments}})
			}
			if msg.Error != "" {
				m.add(block{kind: blockError, text: msg.Error})
			}
		case store.RoleTool:
			if i, ok := m.toolIdx[msg.ToolCallID]; ok {
				t := &m.blocks[i].tool
				t.Result, t.Done = msg.Content, true
			}
		}
	}
	title := th.Title
	if title == "" {
		title = "(untitled)"
	}
	m.add(block{kind: blockNotice, text: "resumed: " + title})
}
