package tui

import (
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/GhaziBenDahmane/harness/hosts/client"
)

// The run options (reasoning effort, plan/build mode) and the plumbing the
// conversation, model and connect commands share: the per-model state, the
// self-applying messages and the keys of the run options.

// extState is the state of those commands that lives with a Model.
type extState struct {
	state     *stateFile // favorite and recent models; nil keeps them in memory
	favorites []string
	recents   []string // most recent first
	connector Connector
}

// selfMsg is a message that knows how to fold itself into the model, so a
// feature file needs no case in Update.
type selfMsg interface{ apply(m *Model) tea.Cmd }

func (m *Model) initExt(o Options) {
	x := &m.ext
	x.state = newStateFile(o.StateDir)
	x.connector = o.Connector
	_ = x.state.get("favorites", &x.favorites)
	_ = x.state.get("recents", &x.recents)
	m.syncPrompt()
}

// initCmds are the commands to run at startup: loading the custom commands.
func (m *Model) initCmds() []tea.Cmd {
	if _, ok := m.be.(CommandBackend); ok {
		return []tea.Cmd{m.loadCommands(false)}
	}
	return nil
}

func init() {
	register(
		command{name: "effort", args: "[low|medium|high|default]", help: "reasoning effort of the next runs (ctrl+t cycles)", run: (*Model).cmdEffort},
		command{name: "plan", help: "plan mode: read-only, the agent proposes a plan (tab toggles)", run: func(m *Model, _ string) tea.Cmd { m.setMode(client.ModePlan); return nil }},
		command{name: "build", help: "build mode: all tools the profile allows (tab toggles)", run: func(m *Model, _ string) tea.Cmd { m.setMode(client.ModeBuild); return nil }},
	)
}

func (m *Model) notice(format string, a ...any) {
	m.add(block{kind: blockNotice, text: fmt.Sprintf(format, a...)})
}

// unsupported reports (and tells the user) that op failed because the backend or server lacks it.
func (m *Model) unsupported(op string, err error) bool {
	if errors.Is(err, client.ErrNotSupported) {
		m.notice("%s: not supported by this backend/server", op)
		return true
	}
	return false
}

func (m *Model) runOpts() (RunOptionsBackend, bool) {
	rb, ok := m.be.(RunOptionsBackend)
	return rb, ok
}

// efforts is the cycle of ctrl+t; "" is the model's default.
var efforts = []string{"", "low", "medium", "high"}

func effortName(e string) string {
	if e == "" {
		return "default"
	}
	return e
}

func (m *Model) cmdEffort(arg string) tea.Cmd {
	rb, ok := m.runOpts()
	if !ok {
		m.notice("/effort: not supported by this backend/server")
		return nil
	}
	switch a := strings.ToLower(arg); a {
	case "":
		m.notice("effort: %s (/effort low|medium|high|default, ctrl+t cycles)", effortName(rb.Effort()))
	case "default", "low", "medium", "high":
		if a == "default" {
			a = ""
		}
		rb.SetEffort(a)
		m.notice("effort: %s", effortName(a))
	default:
		m.notice("/effort: unknown level %q — use low, medium, high or default", arg)
	}
	return nil
}

// cycleEffort moves to the next level of efforts.
func (m *Model) cycleEffort() {
	rb, ok := m.runOpts()
	if !ok {
		m.notice("effort: not supported by this backend/server")
		return
	}
	next := efforts[0]
	for i, e := range efforts {
		if e == rb.Effort() {
			next = efforts[(i+1)%len(efforts)]
		}
	}
	rb.SetEffort(next)
	m.notice("effort: %s", effortName(next))
}

// setMode switches between plan and build and shows it in the input prompt.
func (m *Model) setMode(mode string) {
	rb, ok := m.runOpts()
	if !ok {
		m.notice("/%s: not supported by this backend/server", mode)
		return
	}
	rb.SetMode(mode)
	m.syncPrompt()
	m.notice("mode: %s", mode)
}

// toggleMode flips plan and build (tab, shift+tab).
func (m *Model) toggleMode() {
	rb, _ := m.runOpts()
	if rb.Mode() == client.ModePlan {
		m.setMode(client.ModeBuild)
	} else {
		m.setMode(client.ModePlan)
	}
}

// syncPrompt puts the mode in front of the input's bar ("plan │ ").
func (m *Model) syncPrompt() {
	m.in.Prompt = "│ "
	if rb, ok := m.runOpts(); ok {
		m.in.Prompt = rb.Mode() + " │ "
	}
}

// extKey handles the keys of the run options: tab and shift+tab toggle the
// mode (after the slash completion took tab), ctrl+t cycles the effort, f2 and
// shift+f2 cycle the recent models.
func (m *Model) extKey(s string) (tea.Cmd, bool) {
	switch s {
	case "tab", "shift+tab":
		if _, ok := m.runOpts(); ok {
			m.toggleMode()
			return nil, true
		}
	case "ctrl+t":
		m.cycleEffort()
		return nil, true
	case "f2":
		m.cycleRecent(1)
		return nil, true
	case "shift+f2", "f14": // xterm sends shift+f2 as f14
		m.cycleRecent(-1)
		return nil, true
	}
	return nil, false
}

// extStatus adds the mode and effort to the status line.
func (m *Model) extStatus(sv *StatusView) {
	if rb, ok := m.runOpts(); ok {
		sv.Mode, sv.Effort = rb.Mode(), rb.Effort()
	}
}
