package tui

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/hosts/client"
)

func TestSlashSuggestionsAreNavigatedWithArrowsWrapAndTabCompletesTheSelected(t *testing.T) {
	// Arrange
	m, _ := newModel(&fakeBackend{})
	typeText(m, "/m")
	names := make([]string, len(m.sugg))
	for i, c := range m.sugg {
		names[i] = c.name
	}
	if len(names) < 3 {
		t.Fatalf("need at least three suggestions for /m, got %v", names)
	}

	// Act
	press(m, tea.KeyDown)
	press(m, tea.KeyTab)
	second := m.in.Value()
	m.in.Reset()
	typeText(m, "/m")
	press(m, tea.KeyUp) // wraps to the last one
	press(m, tea.KeyTab)

	// Assert
	if second != "/"+names[1]+" " {
		t.Fatalf("down+tab gave %q, want /%s", second, names[1])
	}
	if got := m.in.Value(); got != "/"+names[len(names)-1]+" " {
		t.Fatalf("up from the first gave %q, want the last suggestion /%s", got, names[len(names)-1])
	}
}

func TestEnterOnASuggestionRunsItAndEscClearsTheInput(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	typeText(m, "/qui")
	press(m, tea.KeyEnter)
	quitRan := m.Quit()
	m2, _ := newModel(&fakeBackend{})
	typeText(m2, "/qui")
	press(m2, tea.KeyEsc)

	if !quitRan {
		t.Fatal("enter on the /quit suggestion did not run it")
	}
	if m2.in.Value() != "" || m2.Quit() {
		t.Fatalf("esc left input %q (quit=%v)", m2.in.Value(), m2.Quit())
	}
}

func TestPagingKeysScrollTheTranscriptAndHomeEndJumpToTheEnds(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	longTranscript(m)
	bottom := m.vp.YOffset

	press(m, tea.KeyPgUp)
	pgup := m.vp.YOffset
	press(m, tea.KeyPgDown)
	afterDown := m.vp.YOffset
	press(m, tea.KeyCtrlU)
	half := m.vp.YOffset
	press(m, tea.KeyCtrlD)
	back := m.vp.YOffset
	press(m, tea.KeyCtrlG)
	top := m.vp.YOffset
	press(m, tea.KeyEnd)
	end := m.vp.YOffset

	if !(pgup < bottom) || afterDown != bottom {
		t.Fatalf("bottom %d, pgup %d, pgdown %d", bottom, pgup, afterDown)
	}
	if !(half < bottom) || back != bottom {
		t.Fatalf("ctrl+u %d, ctrl+d %d (bottom %d)", half, back, bottom)
	}
	if top != 0 || end != bottom {
		t.Fatalf("ctrl+g %d, end %d (bottom %d)", top, end, bottom)
	}
}

func TestHomeAndEndMoveTheCursorInsteadOfScrollingWhenTheInputHasText(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	longTranscript(m)
	bottom := m.vp.YOffset
	typeText(m, "draft")

	press(m, tea.KeyHome)

	if m.vp.YOffset != bottom {
		t.Fatalf("home scrolled the transcript to %d while a draft was being edited", m.vp.YOffset)
	}
}

func TestCtrlOExpandsToolsAndCtrlCQuitsWhileAnApprovalIsWaiting(t *testing.T) {
	// Arrange
	be := &fakeBackend{}
	m, _ := newModel(be)
	startRun(t, m, "clean up")
	feed(t, m, approvalInterrupt())

	// Act
	press(m, tea.KeyCtrlO)
	expanded := m.expanded
	press(m, tea.KeyCtrlC)
	armed := m.quitArmed
	press(m, tea.KeyCtrlC)

	// Assert
	if !expanded || !armed || !m.Quit() || len(be.resum) != 0 {
		t.Fatalf("expanded %v armed %v quit %v resumes %v", expanded, armed, m.Quit(), be.resum)
	}
}

func TestEscDeniesAPendingApproval(t *testing.T) {
	be := &fakeBackend{}
	m, _ := newModel(be)
	startRun(t, m, "clean up")
	feed(t, m, approvalInterrupt())

	press(m, tea.KeyEsc)

	if len(be.resum) != 1 || be.resum[0].Decision != DecisionDeny {
		t.Fatalf("resumes %+v", be.resum)
	}
}

func TestAnyOtherKeyThanTheNumberDoesNotAnswerAnAskUserOption(t *testing.T) {
	be := &fakeBackend{}
	m, _ := newModel(be)
	startRun(t, m, "x")
	feed(t, m, askUserInterrupt())

	typeText(m, "3") // only two options
	typeText(m, "x")

	if len(be.resum) != 0 || m.in.Value() != "3x" {
		t.Fatalf("resumes %+v input %q", be.resum, m.in.Value())
	}
}

func TestSelectionStopsAtTheFirstMessageAndLeavesBySteppingPastTheLast(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	m.add(block{kind: blockUser, text: "one"})
	m.add(block{kind: blockAssistant, text: "two"})
	alt := func(k tea.KeyType) { update(m, tea.KeyMsg{Type: k, Alt: true}) }

	alt(tea.KeyDown) // nothing selected yet: no-op
	none := m.inp.sel
	alt(tea.KeyUp)
	alt(tea.KeyUp)
	alt(tea.KeyUp) // clamps at the first
	first := m.inp.sel
	alt(tea.KeyDown)
	second := m.inp.sel
	alt(tea.KeyDown) // past the last: back to no selection
	left := m.inp.sel

	if none != -1 || first != 0 || second != 1 || left != -1 {
		t.Fatalf("selection path %d %d %d %d", none, first, second, left)
	}
}

func TestSelectionKeysAreIgnoredOnAnEmptyConversationAndWhileTyping(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	update(m, tea.KeyMsg{Type: tea.KeyUp, Alt: true})
	empty := m.inp.sel
	m.add(block{kind: blockUser, text: "one"})
	typeText(m, "typing")

	update(m, tea.KeyMsg{Type: tea.KeyUp, Alt: true})

	if empty != -1 || m.inp.sel != -1 {
		t.Fatalf("selection %d / %d", empty, m.inp.sel)
	}
}

func clipboardText(t *testing.T, osc string) string {
	t.Helper()
	const prefix = "\x1b]52;c;"
	i := strings.Index(osc, prefix)
	if i < 0 {
		t.Fatalf("no OSC 52 in %q", osc)
	}
	enc := osc[i+len(prefix):]
	enc = enc[:strings.IndexByte(enc, '\a')]
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCopyWithoutSelectionTakesTheLastAnswer(t *testing.T) {
	var out bytes.Buffer
	m, _ := newModel(&fakeBackend{})
	m.inp.out = &out
	m.add(block{kind: blockAssistant, text: "first answer"})
	m.add(block{kind: blockUser, text: "more"})
	m.add(block{kind: blockAssistant, text: "last answer"})

	typeText(m, "/copy")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	msgs(cmd)

	if got := clipboardText(t, out.String()); got != "last answer" {
		t.Fatalf("copied %q", got)
	}
	if !strings.Contains(m.Transcript(), "copied 11 characters") {
		t.Fatalf("no confirmation:\n%s", m.Transcript())
	}
}

func TestCopyOfASelectedToolCallIsItsNameArgumentsAndResult(t *testing.T) {
	var out bytes.Buffer
	m, _ := newModel(&fakeBackend{})
	m.inp.out = &out
	startRun(t, m, "look")
	feed(t, m, toolEvents("c1", "read_file", `{"path":"a.go"}`, "package a\n", false)...)
	m.inp.sel = 1

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y"), Alt: true})
	msgs(cmd)

	got := clipboardText(t, out.String())
	if !strings.HasPrefix(got, "read_file") || !strings.Contains(got, "a.go") || !strings.HasSuffix(got, "package a") {
		t.Fatalf("copied %q", got)
	}
}

func TestCopyOfAThinkingBlockIsItsText(t *testing.T) {
	var out bytes.Buffer
	m, _ := newModel(&fakeBackend{})
	m.inp.out = &out
	startRun(t, m, "think")
	feed(t, m,
		agui.New(agui.ReasoningMessageStart, map[string]any{"messageId": "r"}),
		agui.New(agui.ReasoningMessageContent, map[string]any{"messageId": "r", "delta": "weighing options"}))
	m.inp.sel = 1

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y"), Alt: true})
	msgs(cmd)

	if got := clipboardText(t, out.String()); got != "weighing options" {
		t.Fatalf("copied %q", got)
	}
}

func TestCopyWithNothingToCopySaysSoAndWritesNothing(t *testing.T) {
	var out bytes.Buffer
	m, _ := newModel(&fakeBackend{})
	m.inp.out = &out
	m.add(block{kind: blockUser, text: "only a question"})

	typeText(m, "/copy")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	msgs(cmd)

	if out.Len() != 0 || !strings.Contains(m.Transcript(), "nothing to copy") {
		t.Fatalf("out %q:\n%s", out.String(), m.Transcript())
	}
}

func TestTerminalTitleFollowsTheSessionAndRunState(t *testing.T) {
	m := New(&fakeBackend{}, Options{Now: func() time.Time { return t0 }})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	idle := m.inp.title
	typeText2(m, "fix the build")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m.Update(runStartedMsg{runID: "r1", events: make(chan client.Event)})
	running := m.inp.title

	if idle != "harness" || running != "harness · fix the build · running" {
		t.Fatalf("titles %q / %q", idle, running)
	}
}

func typeText2(m *Model, s string) {
	for _, r := range s {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func TestSessionTitleIsTheGeneratedOneElseTheStartOfTheFirstMessage(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	m.add(block{kind: blockAssistant, text: "hello"})
	none := m.sessionTitle()
	m.add(block{kind: blockUser, text: "fix\nthe build"})
	fromMessage := m.sessionTitle()
	startRun(t, m, "go")
	feed(t, m, agui.CustomEvent(agui.CustomTitle, map[string]any{"title": "Build repair"}))

	if none != "" || fromMessage != "fix the build" || m.sessionTitle() != "Build repair" {
		t.Fatalf("titles %q %q %q", none, fromMessage, m.sessionTitle())
	}
}
