package tui

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/GhaziBenDahmane/harness/engine/agui"
	"github.com/GhaziBenDahmane/harness/hosts/client"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files")

func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.Ascii)
	os.Exit(m.Run())
}

// golden compares got with testdata/<name>.golden (or rewrites it with -update).
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	got = strings.TrimRight(got, "\n") + "\n"
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if string(want) != got {
		t.Fatalf("%s differs from the golden file.\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}

// fakeBackend records the calls of the model.
type fakeBackend struct {
	mu    sync.Mutex
	calls []string
	resum []ResumeRequest
	runs  int
}

func (f *fakeBackend) Model() string { return "gpt-test" }
func (f *fakeBackend) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}
func (f *fakeBackend) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}
func (f *fakeBackend) next() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs++
	return "run" + string(rune('0'+f.runs))
}
func (f *fakeBackend) Send(_ context.Context, text string) (string, error) {
	f.record("send:" + text)
	return f.next(), nil
}
func (f *fakeBackend) Events(context.Context, string) <-chan client.Event {
	return make(chan client.Event)
}
func (f *fakeBackend) Steer(_ context.Context, runID, text string) error {
	f.record("steer:" + runID + ":" + text)
	return nil
}
func (f *fakeBackend) Cancel(_ context.Context, runID string) error {
	f.record("cancel:" + runID)
	return nil
}
func (f *fakeBackend) Resume(_ context.Context, r ResumeRequest) (string, error) {
	f.mu.Lock()
	f.resum = append(f.resum, r)
	f.mu.Unlock()
	f.record("resume:" + r.InterruptID)
	return f.next(), nil
}

// ev builds the EventMsg of an AG-UI event.
func ev(t *testing.T, e agui.Event) EventMsg {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return EventMsg{Ev: Decode(client.Event{Type: e.Type(), Data: b})}
}

// msgs runs a command and collects the messages it produces, following
// batches; commands that do not finish quickly (timers, blocking reads) are
// abandoned.
func msgs(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case m := <-ch:
		if b, ok := m.(tea.BatchMsg); ok {
			var out []tea.Msg
			for _, c := range b {
				out = append(out, msgs(c)...)
			}
			return out
		}
		return []tea.Msg{m}
	case <-time.After(100 * time.Millisecond):
		return nil
	}
}

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func newModel(be Backend) (*Model, *time.Time) {
	now := t0
	m := New(be, Options{Now: func() time.Time { return now }, StaticCursor: true})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return m, &now
}

// update feeds a message and then every message its command produces.
func update(m *Model, msg tea.Msg) {
	_, cmd := m.Update(msg)
	for _, r := range msgs(cmd) {
		if _, ok := r.(pumped); ok {
			continue // the event pump is driven by the test
		}
		if _, ok := r.(tickMsg); ok {
			continue
		}
		update(m, r)
	}
}

func typeText(m *Model, s string) {
	for _, r := range s {
		update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func press(m *Model, k tea.KeyType) { update(m, tea.KeyMsg{Type: k}) }

// startRun types text, presses enter and returns once the run is active.
func startRun(t *testing.T, m *Model, text string) {
	t.Helper()
	typeText(m, text)
	press(m, tea.KeyEnter)
	if !m.Running() {
		t.Fatal("the run did not start")
	}
}

func feed(t *testing.T, m *Model, es ...agui.Event) {
	t.Helper()
	for _, e := range es {
		update(m, ev(t, e))
	}
}

func toolEvents(id, name, args, result string, isErr bool) []agui.Event {
	res := map[string]any{"toolCallId": id, "content": result, "role": "tool"}
	if isErr {
		res["metadata"] = map[string]any{"isError": true}
	}
	return []agui.Event{
		agui.New(agui.ToolCallStart, map[string]any{"toolCallId": id, "toolCallName": name}),
		agui.New(agui.ToolCallArgs, map[string]any{"toolCallId": id, "delta": args[:len(args)/2]}),
		agui.New(agui.ToolCallArgs, map[string]any{"toolCallId": id, "delta": args[len(args)/2:]}),
		agui.New(agui.ToolCallEnd, map[string]any{"toolCallId": id}),
		agui.New(agui.ToolCallResult, res),
	}
}

func text(delta string) agui.Event {
	return agui.New(agui.TextMessageContent, map[string]any{"messageId": "m1", "delta": delta})
}

func TestStreamedAssistantTextIsRenderedAsMarkdown(t *testing.T) {
	// Arrange
	m, _ := newModel(&fakeBackend{})
	startRun(t, m, "write a loop")

	// Act
	feed(t, m,
		agui.RunStartedEvent("t", "run1"),
		agui.New(agui.StepStarted, map[string]any{"stepName": "s1"}),
		agui.New(agui.TextMessageStart, map[string]any{"messageId": "m1", "role": "assistant"}),
		text("Here is **the** plan:\n- first `step`\n- second"),
		text("\n\n```go\nfor i := 0; i < 3; i++ {\n"),
	)
	streaming := m.Transcript()
	feed(t, m,
		text("}\n```\nDone."),
		agui.New(agui.TextMessageEnd, map[string]any{"messageId": "m1"}),
	)

	// Assert
	golden(t, "streamed_text_partial", streaming)
	golden(t, "streamed_text", m.Transcript())
}

func TestToolCallStartArgsAndResult(t *testing.T) {
	// Arrange
	m, _ := newModel(&fakeBackend{})
	startRun(t, m, "look around")

	// Act
	feed(t, m, toolEvents("c1", "read_file", `{"path":"src/main.go"}`, "package main\n\nfunc main() {}\n", false)[:2]...)
	running := m.Transcript()
	feed(t, m, toolEvents("c1", "read_file", `{"path":"src/main.go"}`, "package main\n\nfunc main() {}\n", false)[2:]...)
	feed(t, m, toolEvents("c2", "run_command", `{"command":"go test ./..."}`, strings.Repeat("ok  \tpkg\t0.1s\n", 8), false)...)
	collapsed := m.Transcript()
	press(m, tea.KeyCtrlO)
	expanded := m.Transcript()

	// Assert
	golden(t, "tool_running", running)
	golden(t, "tool_collapsed", collapsed)
	golden(t, "tool_expanded", expanded)
}

func TestLiveToolOutputShowsTheLastLinesWhileTheCallRuns(t *testing.T) {
	// Arrange
	m, _ := newModel(&fakeBackend{})
	startRun(t, m, "build")
	feed(t, m, toolEvents("c1", "run_command", `{"command":"make"}`, "x", false)[:1]...)

	// Act
	feed(t, m, agui.CustomEvent(agui.CustomToolOutput, map[string]any{"toolCallId": "c1", "stream": "stdout", "text": "one\ntwo\nthree\nfour\n"}))

	// Assert
	golden(t, "tool_live_output", m.Transcript())
}

func TestEditFileResultShowsADiffOfOldAndNewText(t *testing.T) {
	// Arrange
	m, _ := newModel(&fakeBackend{})
	startRun(t, m, "fix it")
	args := `{"path":"a.go","oldText":"func a() int {\n\treturn 1\n}","newText":"func a() int {\n\treturn 2\n}"}`

	// Act
	feed(t, m, toolEvents("c1", "edit_file", args, `{"path":"a.go","replacements":1,"matched":"exact"}`, false)...)

	// Assert
	golden(t, "edit_diff", m.Transcript())
}

func TestErrorToolResultAndRunErrorAreRenderedAsErrors(t *testing.T) {
	// Arrange
	m, _ := newModel(&fakeBackend{})
	startRun(t, m, "go")

	// Act
	feed(t, m, toolEvents("c1", "read_file", `{"path":"nope.txt"}`, "file not found: nope.txt", true)...)
	feed(t, m, agui.RunErrorEvent(agui.CodeModelError, "the model is unavailable"))

	// Assert
	golden(t, "errors", m.Transcript())
	if m.Running() {
		t.Fatal("a failed run must not stay active")
	}
}

func TestStatusLineShowsModelStepsTokensCacheAndElapsed(t *testing.T) {
	// Arrange
	m, now := newModel(&fakeBackend{})
	startRun(t, m, "hi")

	// Act
	feed(t, m,
		agui.New(agui.StepStarted, map[string]any{"stepName": "s1"}),
		agui.New(agui.StepStarted, map[string]any{"stepName": "s2"}),
		agui.CustomEvent(agui.CustomUsage, map[string]any{"inputTokens": 12000.0, "outputTokens": 340.0, "cachedInputTokens": 9000.0}),
	)
	*now = t0.Add(75 * time.Second)
	update(m, tickMsg(*now))
	live := m.View()
	feed(t, m, agui.RunFinishedEvent("t", "run1", "success", []agui.Usage{{Model: "gpt-test", InputTokens: 12500, OutputTokens: 400, CachedInputTokens: 10000}}))

	// Assert
	golden(t, "view_running", live)
	golden(t, "view_finished", m.View())
}

func TestEnterSendsWhenIdleAndSteersWhileARunIsActive(t *testing.T) {
	// Arrange
	fb := &fakeBackend{}
	m, _ := newModel(fb)

	// Act
	startRun(t, m, "first")
	typeText(m, "use blue")
	press(m, tea.KeyEnter)
	feed(t, m, agui.RunFinishedEvent("t", "run1", "success", nil))
	startRun(t, m, "second")

	// Assert
	want := []string{"send:first", "steer:run1:use blue", "send:second"}
	if got := fb.Calls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v, want %v", got, want)
	}
	golden(t, "send_and_steer", m.Transcript())
}

func TestUndeliveredSteerIsMarked(t *testing.T) {
	// Arrange
	m, _ := newModel(&fakeBackend{})
	startRun(t, m, "go")
	typeText(m, "too late")
	press(m, tea.KeyEnter)

	// Act
	feed(t, m, agui.SteerEvent("s1", "too late", false))

	// Assert
	if !strings.Contains(m.Transcript(), "not delivered") {
		t.Fatalf("transcript %q", m.Transcript())
	}
}

func TestEscCancelsTheRunAndCtrlCTwiceQuits(t *testing.T) {
	// Arrange
	fb := &fakeBackend{}
	m, _ := newModel(fb)
	startRun(t, m, "long task")

	// Act
	press(m, tea.KeyEsc)
	feed(t, m, agui.RunFinishedEvent("t", "run1", "cancelled", nil))
	afterCancel := m.Transcript()
	startRun(t, m, "again")
	press(m, tea.KeyCtrlC) // cancels the second run
	quitAfterFirst := m.Quit()
	press(m, tea.KeyCtrlC) // second: quit

	// Assert
	if got := strings.Join(fb.Calls(), "|"); got != "send:long task|cancel:run1|send:again|cancel:run2" {
		t.Fatalf("calls %s", got)
	}
	if quitAfterFirst || !m.Quit() {
		t.Fatalf("quit after first=%v after second=%v", quitAfterFirst, m.Quit())
	}
	golden(t, "cancelled", afterCancel)
}

func TestCtrlCWhenIdleNeedsConfirmation(t *testing.T) {
	// Arrange
	m, _ := newModel(&fakeBackend{})

	// Act
	press(m, tea.KeyCtrlC)
	hint := m.View()
	first := m.Quit()
	press(m, tea.KeyCtrlC)

	// Assert
	if first || !m.Quit() || !strings.Contains(hint, "ctrl+c again") {
		t.Fatalf("first=%v second=%v hint=%q", first, m.Quit(), hint)
	}
}

func approvalInterrupt() agui.Event {
	return agui.RunInterruptedEvent("t", "run1", []agui.Interrupt{{
		ID: "i1", Reason: client.ReasonToolApproval, Message: "run_command", ToolCallID: "c1",
		ResponseSchema: map[string]any{"allowFreeText": true},
		Approval:       map[string]any{"id": "ap1", "toolName": "run_command", "toolInputPreview": `{"command":"rm -rf build"}`},
	}}, nil)
}

func TestApprovalPromptIsShownAndResumesWithTheDecision(t *testing.T) {
	cases := map[string]struct {
		key  rune
		want ResumeRequest
	}{
		"approve once":   {'y', ResumeRequest{InterruptID: "ap1", Reason: "tool_approval", Decision: DecisionApprove}},
		"approve always": {'a', ResumeRequest{InterruptID: "ap1", Reason: "tool_approval", Decision: DecisionApprove, Always: true}},
		"deny":           {'n', ResumeRequest{InterruptID: "ap1", Reason: "tool_approval", Decision: DecisionDeny}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			fb := &fakeBackend{}
			m, _ := newModel(fb)
			startRun(t, m, "clean up")
			feed(t, m, approvalInterrupt())
			prompt := m.View()

			// Act
			update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{tc.key}})

			// Assert
			if len(fb.resum) != 1 || fb.resum[0] != tc.want {
				t.Fatalf("resume %+v, want %+v", fb.resum, tc.want)
			}
			if !m.Running() {
				t.Fatal("the resume run should be active")
			}
			if name == "approve once" {
				golden(t, "approval_prompt", prompt)
			}
		})
	}
}

func TestApprovalPromptSwallowsTyping(t *testing.T) {
	// Arrange
	fb := &fakeBackend{}
	m, _ := newModel(fb)
	startRun(t, m, "x")
	feed(t, m, approvalInterrupt())

	// Act
	typeText(m, "hello")
	press(m, tea.KeyEnter)

	// Assert
	if len(fb.resum) != 0 || m.in.Value() != "" {
		t.Fatalf("resume %v input %q", fb.resum, m.in.Value())
	}
}

func askUserInterrupt() agui.Event {
	return agui.RunInterruptedEvent("t", "run1", []agui.Interrupt{{
		ID: "q1", Reason: "ask_user", Message: "Which database?", ToolCallID: "c1",
		ResponseSchema: map[string]any{"allowFreeText": true, "options": []map[string]any{{"label": "Postgres"}, {"label": "SQLite"}}},
	}}, nil)
}

func TestAskUserPromptTakesAnOptionNumberOrFreeText(t *testing.T) {
	t.Run("option", func(t *testing.T) {
		// Arrange
		fb := &fakeBackend{}
		m, _ := newModel(fb)
		startRun(t, m, "x")
		feed(t, m, askUserInterrupt())
		prompt := m.View()

		// Act
		typeText(m, "2")

		// Assert
		if len(fb.resum) != 1 || fb.resum[0].Text != "SQLite" || fb.resum[0].InterruptID != "q1" || fb.resum[0].Reason != "ask_user" {
			t.Fatalf("resume %+v", fb.resum)
		}
		golden(t, "ask_user_prompt", prompt)
	})
	t.Run("free text", func(t *testing.T) {
		// Arrange
		fb := &fakeBackend{}
		m, _ := newModel(fb)
		startRun(t, m, "x")
		feed(t, m, askUserInterrupt())

		// Act
		typeText(m, "MySQL please")
		press(m, tea.KeyEnter)

		// Assert
		if len(fb.resum) != 1 || fb.resum[0].Text != "MySQL please" {
			t.Fatalf("resume %+v", fb.resum)
		}
	})
}

func TestMarkdownStylesBoldAndCodeWhenColorIsAvailable(t *testing.T) {
	// Arrange
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })
	bold := lipgloss.NewStyle().Bold(true).Render("x")

	// Act
	got := RenderMarkdown("a **x** b", 40)

	// Assert
	if !strings.Contains(got, bold) || strings.Contains(got, "**") {
		t.Fatalf("got %q", got)
	}
}

func TestRenderDiffKeepsCommonLinesAsContext(t *testing.T) {
	// Act
	got := RenderDiff("f.go", "a\nb\nc", "a\nB\nc")

	// Assert
	if want := "@@ f.go @@\n a\n-b\n+B\n c"; got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}
