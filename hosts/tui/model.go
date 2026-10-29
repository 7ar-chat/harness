package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/GhaziBenDahmane/harness/engine/agui"
	"github.com/GhaziBenDahmane/harness/hosts/client"
)

// Messages the model handles besides key and window messages.
type (
	// EventMsg carries one event of the active run.
	EventMsg struct {
		Run string
		Ev  Ev
	}
	// streamDoneMsg: the event channel of a run was closed.
	streamDoneMsg struct{ run string }
	// runStartedMsg: Send or Resume created a run.
	runStartedMsg struct {
		runID  string
		events <-chan client.Event
		err    error
	}
	// tickMsg refreshes the elapsed time.
	tickMsg time.Time
	// opErrMsg reports a failed Steer or Cancel.
	opErrMsg struct {
		op  string
		err error
	}
)

type blockKind int

const (
	blockUser blockKind = iota
	blockAssistant
	blockTool
	blockReasoning
	blockSteer
	blockError
	blockNotice
)

type block struct {
	kind      blockKind
	text      string
	tool      ToolView
	id        string // tool call id
	delivered bool   // steer
	open      bool   // assistant/reasoning message still streaming
	at        time.Time
}

// pending is the interrupt the last run ended with.
type pending struct {
	prompt Prompt
	id     string
}

// Options configure New.
type Options struct {
	// Now is the clock (default time.Now); tests inject a fixed one.
	Now func() time.Time
	// Context is used for the backend calls (default Background).
	Context context.Context
	// StaticCursor disables cursor blinking (tests).
	StaticCursor bool
	// Workspace is the local project directory: the root of @file mentions and
	// the working directory of !commands. "" (a remote session) turns both off.
	Workspace string
	// StateDir keeps the input history and the stash (~/.har); "" keeps them in memory.
	StateDir string
	// Attention configures notifications while the terminal is in the background.
	Attention Attention
}

// Model is the Bubble Tea model of the terminal client. It is pure with
// respect to the world: events arrive as messages (from a channel of
// client.Event pumped by a command), and every effect goes through Backend.
type Model struct {
	be  Backend
	ctx context.Context
	now func() time.Time

	width, height int
	vp            viewport.Model
	in            textarea.Model

	blocks   []block
	toolIdx  map[string]int
	expanded bool
	ext      ext // input and chrome features (ext.go)

	picker  *picker   // open list (/model, /sessions), nil when none
	sugg    []command // slash completions of the input
	suggSel int

	runID      string
	starting   bool // Send/Resume in flight
	running    bool
	cancelling bool
	quitArmed  bool
	runStart   time.Time
	elapsed    time.Duration
	steps      int
	base, cur  Usage
	pending    *pending
	quit       bool
	static     bool
}

// New returns the model for a backend.
func New(be Backend, o Options) *Model {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Context == nil {
		o.Context = context.Background()
	}
	in := textarea.New()
	in.Prompt = "│ "
	in.Placeholder = "Message (enter sends, alt+enter or ctrl+j adds a line)"
	in.ShowLineNumbers = false
	in.CharLimit = 0
	in.SetHeight(3)
	in.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	if o.StaticCursor {
		in.Cursor.SetMode(cursor.CursorStatic)
	}
	in.Focus()
	m := &Model{static: o.StaticCursor, be: be, ctx: o.Context, now: o.Now, in: in, toolIdx: map[string]int{}, width: 80, height: 24}
	m.ext = newExt(o)
	m.vp = viewport.New(80, 10)
	m.layout()
	return m
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd {
	if m.static {
		return nil
	}
	return textarea.Blink
}

// Quit reports whether the model asked to leave.
func (m *Model) Quit() bool { return m.quit }

// Running reports whether a run is active.
func (m *Model) Running() bool { return m.running }

// Transcript is the rendered conversation (what the viewport shows).
func (m *Model) Transcript() string { return m.renderBlocks() }

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	if cmd, ok := m.extraUpdate(msg); ok {
		m.refresh()
		return m, tea.Batch(cmd, m.afterUpdate())
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
	case tea.KeyMsg:
		if cmd, handled := m.key(msg); handled {
			m.refresh()
			return m, tea.Batch(cmd, m.afterUpdate())
		}
		var cmd tea.Cmd
		m.in, cmd = m.in.Update(msg)
		cmds = append(cmds, cmd)
	case runStartedMsg:
		m.starting = false
		if msg.err != nil {
			m.add(block{kind: blockError, text: msg.err.Error()})
			break
		}
		m.runID, m.running, m.cancelling, m.pending = msg.runID, true, false, nil
		m.runStart, m.elapsed, m.steps, m.cur = m.now(), 0, 0, Usage{}
		cmds = append(cmds, pump(msg.runID, msg.events), tick())
	case EventMsg:
		if msg.Run == m.runID || msg.Run == "" {
			m.apply(msg.Ev)
		}
	case pumped:
		if msg.Run == m.runID {
			m.apply(msg.Ev)
		}
		cmds = append(cmds, msg.next)
	case streamDoneMsg:
		if msg.run == m.runID && m.running {
			m.finishRun()
			m.add(block{kind: blockNotice, text: "the event stream ended without a result"})
		}
	case tickMsg:
		if m.running {
			m.elapsed = m.now().Sub(m.runStart)
			cmds = append(cmds, tick())
		}
	case opErrMsg:
		m.add(block{kind: blockNotice, text: msg.op + ": " + msg.err.Error()})
	case modelsMsg:
		if msg.err != nil && len(msg.ids) == 0 {
			m.add(block{kind: blockNotice, text: "models: " + msg.err.Error() + " — /model <id> sets one by hand"})
			break
		}
		m.openModelPicker(msg.ids)
	case sessionsMsg:
		if msg.err != nil {
			m.add(block{kind: blockNotice, text: "sessions: " + msg.err.Error()})
			break
		}
		m.openSessionPicker(msg.list)
	case threadMsg:
		if msg.err != nil {
			m.add(block{kind: blockError, text: msg.err.Error()})
			break
		}
		m.loadThread(msg.th)
		m.vp.GotoBottom()
	default:
		var cmd tea.Cmd
		m.in, cmd = m.in.Update(msg)
		cmds = append(cmds, cmd)
	}
	m.refresh()
	cmds = append(cmds, m.afterUpdate())
	return m, tea.Batch(cmds...)
}

// key handles the keys with a meaning of their own; handled=false passes the
// key to the input box.
func (m *Model) key(k tea.KeyMsg) (tea.Cmd, bool) {
	s := k.String()
	if s != "ctrl+c" {
		m.quitArmed = false
	}
	if p := m.picker; p != nil {
		done, it := p.key(k)
		if done {
			m.picker = nil
		}
		if it != nil {
			return p.pick(m, *it), true
		}
		return nil, true
	}
	if cmd, ok := m.extraKey(k); ok {
		return cmd, true
	}
	if len(m.sugg) > 0 {
		switch s {
		case "up":
			m.suggSel = (m.suggSel + len(m.sugg) - 1) % len(m.sugg)
			return nil, true
		case "down":
			m.suggSel = (m.suggSel + 1) % len(m.sugg)
			return nil, true
		case "tab":
			m.in.SetValue("/" + m.sugg[m.suggSel].name + " ")
			m.in.CursorEnd()
			return nil, true
		case "enter":
			return m.sugg[m.suggSel].run(m, ""), m.clearInput()
		case "esc":
			m.in.Reset()
			return nil, true
		}
	}
	// Approval prompt: single keys.
	if p := m.pending; p != nil && p.prompt.Reason == client.ReasonToolApproval {
		switch s {
		case "y", "Y":
			return m.resume(ResumeRequest{Decision: DecisionApprove}), true
		case "a", "A":
			return m.resume(ResumeRequest{Decision: DecisionApprove, Always: true}), true
		case "n", "N", "esc":
			return m.resume(ResumeRequest{Decision: DecisionDeny}), true
		case "ctrl+c":
		case "ctrl+o", "pgup", "pgdown":
		default:
			return nil, true // swallow typing while an approval waits
		}
	}
	switch s {
	case "ctrl+c":
		switch {
		case m.running && !m.cancelling:
			return m.cancel(), true
		case m.quitArmed || m.cancelling:
			m.quit = true
			return tea.Quit, true
		default:
			m.quitArmed = true
			return nil, true
		}
	case "esc":
		if m.running && !m.cancelling {
			return m.cancel(), true
		}
		return nil, true
	case "enter":
		return m.submit(), true
	case "ctrl+o":
		m.expanded = !m.expanded
		return nil, true
	case "pgup", "pgdown", "ctrl+u", "ctrl+d":
		var cmd tea.Cmd
		kk := k
		switch s {
		case "ctrl+u":
			kk = tea.KeyMsg{Type: tea.KeyPgUp}
		case "ctrl+d":
			kk = tea.KeyMsg{Type: tea.KeyPgDown}
		}
		m.vp, cmd = m.vp.Update(kk)
		return cmd, true
	}
	// ask_user options by number when the input is empty.
	if p := m.pending; p != nil && p.prompt.Reason != client.ReasonToolApproval && m.in.Value() == "" && len(s) == 1 {
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(p.prompt.Options) {
			return m.resume(ResumeRequest{Text: p.prompt.Options[n-1]}), true
		}
	}
	return nil, false
}

// submit handles Enter: answer a question, steer the active run, or send.
func (m *Model) submit() tea.Cmd {
	text := strings.TrimSpace(m.in.Value())
	if text == "" || m.starting {
		return nil
	}
	if cmd, ok := m.preSubmit(m.in.Value()); ok {
		return cmd
	}
	if m.pending == nil {
		if cmd, ok := m.slash(m.in.Value()); ok {
			return cmd
		}
	}
	if p := m.pending; p != nil {
		if p.prompt.Reason == client.ReasonToolApproval {
			return nil
		}
		return m.resume(ResumeRequest{Text: text})
	}
	m.in.Reset()
	payload := m.compose(text)
	if m.running {
		m.add(block{kind: blockSteer, text: text, delivered: true})
		runID := m.runID
		return func() tea.Msg {
			if err := m.be.Steer(m.ctx, runID, payload); err != nil {
				return opErrMsg{"steer", err}
			}
			return nil
		}
	}
	m.add(block{kind: blockUser, text: text})
	m.starting = true
	return m.start(func() (string, error) { return m.be.Send(m.ctx, payload) })
}

// resume answers the pending interrupt.
func (m *Model) resume(r ResumeRequest) tea.Cmd {
	p := m.pending
	if p == nil {
		return nil
	}
	r.InterruptID, r.Reason = p.id, p.prompt.Reason
	switch {
	case r.Reason == client.ReasonToolApproval && r.Decision == DecisionApprove && r.Always:
		m.add(block{kind: blockNotice, text: "approved " + p.prompt.Title + " for this run"})
	case r.Reason == client.ReasonToolApproval && r.Decision == DecisionApprove:
		m.add(block{kind: blockNotice, text: "approved " + p.prompt.Title})
	case r.Reason == client.ReasonToolApproval:
		m.add(block{kind: blockNotice, text: "denied " + p.prompt.Title})
	default:
		m.add(block{kind: blockUser, text: r.Text})
	}
	m.in.Reset()
	m.pending, m.starting = nil, true
	return m.start(func() (string, error) { return m.be.Resume(m.ctx, r) })
}

// start runs a backend call that creates a run, then opens its event stream.
func (m *Model) start(create func() (string, error)) tea.Cmd {
	return func() tea.Msg {
		id, err := create()
		if err != nil {
			return runStartedMsg{err: err}
		}
		return runStartedMsg{runID: id, events: m.be.Events(m.ctx, id)}
	}
}

func (m *Model) cancel() tea.Cmd {
	if m.runID == "" {
		return nil
	}
	m.cancelling = true
	runID := m.runID
	return func() tea.Msg {
		if err := m.be.Cancel(m.ctx, runID); err != nil {
			return opErrMsg{"cancel", err}
		}
		return nil
	}
}

// pump reads one event of the channel; Update re-arms it after each event.
func pump(run string, ch <-chan client.Event) tea.Cmd {
	var next tea.Cmd
	next = func() tea.Msg {
		se, ok := <-ch
		if !ok {
			return streamDoneMsg{run}
		}
		return pumped{EventMsg{Run: run, Ev: Decode(se)}, next}
	}
	return next
}

// pumped is an event plus the command that reads the next one.
type pumped struct {
	EventMsg
	next tea.Cmd
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// finishRun marks the run as over and folds its usage into the session total.
func (m *Model) finishRun() {
	if m.running {
		m.elapsed = m.now().Sub(m.runStart)
	}
	m.running, m.cancelling = false, false
	m.base.add(m.cur)
	m.cur = Usage{}
	for i := range m.blocks {
		m.blocks[i].open = false
	}
}

func (u *Usage) add(o Usage) {
	u.In += o.In
	u.Out += o.Out
	u.Cached += o.Cached
	u.Reasoning += o.Reasoning
}

func (m *Model) add(b block) {
	b.at = m.now()
	m.blocks = append(m.blocks, b)
}

// last returns the index of the last block of kind that is still open, or -1.
func (m *Model) openBlock(kind blockKind) int {
	for i := len(m.blocks) - 1; i >= 0 && i >= len(m.blocks)-3; i-- {
		if m.blocks[i].kind == kind && m.blocks[i].open {
			return i
		}
	}
	return -1
}

// apply folds one AG-UI event into the transcript and status.
func (m *Model) apply(ev Ev) {
	defer m.observe(ev)
	switch ev.Type {
	case agui.StepStarted:
		m.steps++
	case agui.TextMessageStart:
		m.add(block{kind: blockAssistant, open: true})
	case agui.TextMessageContent:
		i := m.openBlock(blockAssistant)
		if i < 0 {
			m.add(block{kind: blockAssistant, open: true})
			i = len(m.blocks) - 1
		}
		m.blocks[i].text += ev.Delta
	case agui.TextMessageEnd:
		if i := m.openBlock(blockAssistant); i >= 0 {
			m.blocks[i].open = false
		}
	case agui.ReasoningMessageStart:
		m.add(block{kind: blockReasoning, open: true})
	case agui.ReasoningMessageContent:
		if i := m.openBlock(blockReasoning); i >= 0 {
			m.blocks[i].tool.Output += ev.Delta // char count only; the text stays hidden
		}
	case agui.ReasoningMessageEnd, agui.ReasoningEnd:
		if i := m.openBlock(blockReasoning); i >= 0 {
			m.blocks[i].open = false
		}
	case agui.ToolCallStart:
		m.toolIdx[ev.ToolCallID] = len(m.blocks)
		m.add(block{kind: blockTool, id: ev.ToolCallID, tool: ToolView{Name: ev.ToolName}})
	case agui.ToolCallArgs:
		if i, ok := m.toolIdx[ev.ToolCallID]; ok {
			m.blocks[i].tool.Args += ev.Delta
		}
	case agui.ToolCallResult:
		if i, ok := m.toolIdx[ev.ToolCallID]; ok {
			t := &m.blocks[i].tool
			t.Result, t.Done, t.Err = ev.Content, true, ev.IsError
		}
	case agui.Custom:
		m.custom(ev)
	case agui.RunError:
		m.finishRun()
		m.add(block{kind: blockError, text: RenderRunError(ev.Code, ev.Message)})
	case agui.RunFinished:
		if len(ev.Usage) > 0 {
			m.cur = SumUsage(ev.Usage)
		}
		m.finishRun()
		switch ev.Outcome {
		case "cancelled":
			m.add(block{kind: blockNotice, text: "run cancelled"})
		case "interrupt":
			m.interrupt(ev)
		}
	}
}

func (m *Model) custom(ev Ev) {
	switch ev.Name {
	case agui.CustomUsage:
		m.cur = ev.UsageValue()
	case agui.CustomToolOutput:
		id, _ := ev.Value["toolCallId"].(string)
		text, _ := ev.Value["text"].(string)
		if i, ok := m.toolIdx[id]; ok {
			m.blocks[i].tool.Output += text
		}
	case agui.CustomSteer:
		if d, _ := ev.Value["delivered"].(bool); !d {
			text, _ := ev.Value["text"].(string)
			// mark the matching local steer block as not delivered
			for i := len(m.blocks) - 1; i >= 0; i-- {
				if m.blocks[i].kind == blockSteer && m.blocks[i].text == text {
					m.blocks[i].delivered = false
					break
				}
			}
		}
	case agui.CustomCompaction:
		if ev.Value["phase"] == "completed" {
			m.add(block{kind: blockNotice, text: "context compacted"})
		}
	}
}

func (m *Model) interrupt(ev Ev) {
	if len(ev.Interrupts) == 0 {
		return
	}
	in := ev.Interrupts[0]
	p := Prompt{Reason: in.Reason, Title: in.Message}
	id := in.ID
	if in.Reason == client.ReasonToolApproval {
		if a := in.Approval; a != nil {
			if v, _ := a["id"].(string); v != "" {
				id = v
			}
			p.Title, _ = a["toolName"].(string)
			p.Detail, _ = a["toolInputPreview"].(string)
		}
	} else {
		p.FreeText, _ = in.ResponseSchema["allowFreeText"].(bool)
		if opts, ok := in.ResponseSchema["options"].([]any); ok {
			for _, o := range opts {
				if mo, ok := o.(map[string]any); ok {
					if l, _ := mo["label"].(string); l != "" {
						p.Options = append(p.Options, l)
					}
				}
			}
		}
	}
	m.pending = &pending{prompt: p, id: id}
}

// renderBlocks renders the whole transcript.
func (m *Model) renderBlocks() string {
	w := m.contentWidth()
	parts := make([]string, 0, len(m.blocks))
	for i := range m.blocks {
		parts = append(parts, m.renderBlock(i, w))
	}
	return strings.Join(parts, "\n\n")
}

func (m *Model) statusView() StatusView {
	u := m.base
	u.add(m.cur)
	sv := StatusView{Model: m.be.Model(), State: "idle", Steps: m.steps, Usage: u}
	switch {
	case m.cancelling:
		sv.State = "cancelling"
	case m.running:
		sv.State = "running"
	case m.starting:
		sv.State = "starting"
	case m.pending != nil:
		sv.State = "waiting"
	}
	if m.runID != "" {
		el := m.elapsed
		sv.Elapsed = fmtDuration(el)
	}
	switch {
	case m.quitArmed:
		sv.Hint = "press ctrl+c again to quit"
	case m.running:
		sv.Hint = "enter steers · esc cancels"
	default:
		sv.Hint = "ctrl+o expands tools · ctrl+c quits"
	}
	return sv
}

func fmtDuration(d time.Duration) string {
	s := int(d.Seconds())
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// layout sizes the viewport and the input to the window.
func (m *Model) layout() {
	w := max(m.width, 20)
	m.in.SetWidth(w)
	promptH := 0
	if m.pending != nil {
		promptH = strings.Count(RenderPrompt(m.pending.prompt, w), "\n") + 2
	}
	if o := m.overlay(w); o != "" {
		promptH += strings.Count(o, "\n") + 2
	}
	m.vp.Width = m.contentWidth()
	m.vp.Height = max(m.height-1-m.in.Height()-promptH, 1)
}

// refresh re-renders the transcript into the viewport, keeping the bottom in
// view when it was there.
func (m *Model) refresh() {
	m.sugg = nil
	if m.picker == nil && m.pending == nil {
		m.sugg = suggest(m.in.Value())
	}
	m.suggSel = min(m.suggSel, max(len(m.sugg)-1, 0))
	m.layout()
	atBottom := m.vp.AtBottom()
	m.vp.SetContent(m.content())
	if atBottom {
		m.vp.GotoBottom()
	}
}

// View implements tea.Model.
func (m *Model) View() string {
	var b strings.Builder
	b.WriteString(m.withSidebar(m.vp.View()))
	b.WriteString("\n")
	if m.pending != nil {
		b.WriteString(RenderPrompt(m.pending.prompt, max(m.width, 20)))
		b.WriteString("\n\n")
	}
	if o := m.overlay(max(m.width, 20)); o != "" {
		b.WriteString(o)
		b.WriteString("\n\n")
	}
	b.WriteString(RenderStatus(m.statusView(), max(m.width, 20)))
	b.WriteString("\n")
	b.WriteString(m.in.View())
	return b.String()
}

// overlay is the picker or the slash completion list shown above the status line.
func (m *Model) overlay(w int) string {
	switch {
	case m.picker != nil:
		return m.picker.view(w)
	case len(m.sugg) > 0:
		return renderSuggestions(m.sugg, m.suggSel, w)
	}
	return m.extraOverlay(w)
}

// clearInput empties the input box (the command being run replaces what was typed); it reports true.
func (m *Model) clearInput() bool { m.in.Reset(); return true }
