package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/hosts/client"
)

// flakyBackend fails the calls a test names.
type flakyBackend struct {
	fakeBackend
	sendErr, steerErr, cancelErr, resumeErr error
}

func (f *flakyBackend) Send(ctx context.Context, text string) (string, error) {
	if f.sendErr != nil {
		return "", f.sendErr
	}
	return f.fakeBackend.Send(ctx, text)
}
func (f *flakyBackend) Steer(ctx context.Context, run, text string) error {
	if f.steerErr != nil {
		return f.steerErr
	}
	return f.fakeBackend.Steer(ctx, run, text)
}
func (f *flakyBackend) Cancel(ctx context.Context, run string) error {
	if f.cancelErr != nil {
		return f.cancelErr
	}
	return f.fakeBackend.Cancel(ctx, run)
}
func (f *flakyBackend) Resume(ctx context.Context, r ResumeRequest) (string, error) {
	if f.resumeErr != nil {
		return "", f.resumeErr
	}
	return f.fakeBackend.Resume(ctx, r)
}

func TestFailedSendShowsTheErrorAndLeavesTheModelReadyForTheNextMessage(t *testing.T) {
	// Arrange
	be := &flakyBackend{sendErr: errors.New("queue full")}
	m, _ := newModel(be)

	// Act
	typeText(m, "hello")
	press(m, tea.KeyEnter)
	afterFailure := m.Transcript()
	be.sendErr = nil
	startRun(t, m, "again")

	// Assert
	if !strings.Contains(afterFailure, "queue full") || strings.Contains(afterFailure, "run cancelled") {
		t.Fatalf("transcript %q", afterFailure)
	}
	if got := lastSend(&be.fakeBackend); got != "again" {
		t.Fatalf("the retry was not sent: %q", got)
	}
}

func TestFailedResumeShowsTheErrorAndReleasesTheModel(t *testing.T) {
	// Arrange
	be := &flakyBackend{resumeErr: errors.New("approval expired")}
	m, _ := newModel(be)
	startRun(t, m, "clean up")
	feed(t, m, approvalInterrupt())

	// Act
	update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})

	// Assert
	if !strings.Contains(m.Transcript(), "approval expired") || m.Running() || m.starting {
		t.Fatalf("running %v starting %v:\n%s", m.Running(), m.starting, m.Transcript())
	}
}

func TestFailedSteerAndCancelAreReportedAsNotices(t *testing.T) {
	// Arrange
	be := &flakyBackend{steerErr: client.ErrRunNotActive, cancelErr: errors.New("server gone")}
	m, _ := newModel(be)
	startRun(t, m, "long task")

	// Act
	typeText(m, "faster")
	press(m, tea.KeyEnter)
	press(m, tea.KeyEsc)

	// Assert
	tr := m.Transcript()
	if !strings.Contains(tr, "steer: ") || !strings.Contains(tr, "cancel: server gone") {
		t.Fatalf("transcript %q", tr)
	}
}

func TestEscWhileCancellingDoesNotCancelTwiceAndCtrlCQuits(t *testing.T) {
	// Arrange
	be := &fakeBackend{}
	m, _ := newModel(be)
	startRun(t, m, "long task")

	// Act
	press(m, tea.KeyEsc)
	press(m, tea.KeyEsc)
	state := m.statusView().State
	press(m, tea.KeyCtrlC)

	// Assert
	if got := strings.Join(be.Calls(), "|"); got != "send:long task|cancel:run1" {
		t.Fatalf("calls %s", got)
	}
	if state != "cancelling" || !m.Quit() {
		t.Fatalf("state %q quit %v", state, m.Quit())
	}
}

func TestEscAndEnterDoNothingWhenIdleOrEmpty(t *testing.T) {
	be := &fakeBackend{}
	m, _ := newModel(be)

	press(m, tea.KeyEsc)
	press(m, tea.KeyEnter)
	typeText(m, "   ")
	press(m, tea.KeyEnter)

	if len(be.Calls()) != 0 || m.Running() || len(m.blocks) != 0 {
		t.Fatalf("calls %v blocks %d", be.Calls(), len(m.blocks))
	}
}

func TestEnterWhileASendIsInFlightIsIgnored(t *testing.T) {
	// Arrange
	be := &fakeBackend{}
	m, _ := newModel(be)
	typeText(m, "first")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}) // the send command has not run yet
	typeText(m, "second")

	// Act
	press(m, tea.KeyEnter)

	// Assert
	if len(be.Calls()) != 0 || m.in.Value() != "second" {
		t.Fatalf("calls %v input %q", be.Calls(), m.in.Value())
	}
	for _, r := range msgs(cmd) {
		update(m, r)
	}
	if got := be.Calls(); len(got) != 1 || got[0] != "send:first" {
		t.Fatalf("calls %v", got)
	}
}

// drain reads the stream through the model the way Bubble Tea does: each
// pumped event is applied and its next read is run, until the stream is done.
func drain(m *Model, run string, ch <-chan client.Event) {
	next := pump(run, ch)
	for next != nil {
		msg := next()
		m.Update(msg)
		next = nil
		if p, ok := msg.(pumped); ok {
			next = p.next
		}
		if _, ok := msg.(streamDoneMsg); ok {
			return
		}
	}
}

func wire(t *testing.T, es ...agui.Event) chan client.Event {
	t.Helper()
	ch := make(chan client.Event, len(es))
	for _, e := range es {
		ch <- client.Event{Type: e.Type(), Data: ev(t, e).Ev.Raw}
	}
	close(ch)
	return ch
}

func TestStartedRunStreamsItsEventsIntoTheTranscriptAndEndsWithTheTerminalEvent(t *testing.T) {
	// Arrange
	m, _ := newModel(&fakeBackend{})
	ch := wire(t,
		agui.New(agui.TextMessageStart, map[string]any{"messageId": "m1"}),
		text("hello "), text("world"),
		agui.RunFinishedEvent("t", "r1", "success", nil))

	// Act
	m.Update(runStartedMsg{runID: "r1", events: ch})
	drain(m, "r1", ch)

	// Assert
	if m.Running() || !strings.Contains(m.Transcript(), "hello world") {
		t.Fatalf("running %v:\n%s", m.Running(), m.Transcript())
	}
	if strings.Contains(m.Transcript(), "without a result") {
		t.Fatalf("a finished run is not a lost stream:\n%s", m.Transcript())
	}
}

func TestEventsOfAnotherRunAreDropped(t *testing.T) {
	// Arrange
	m, _ := newModel(&fakeBackend{})
	m.Update(runStartedMsg{runID: "r2", events: make(chan client.Event)})
	ch := wire(t, text("stale answer"), agui.RunFinishedEvent("t", "r1", "success", nil))

	// Act
	drain(m, "r1", ch)

	// Assert
	if !m.Running() || strings.Contains(m.Transcript(), "stale answer") {
		t.Fatalf("running %v:\n%s", m.Running(), m.Transcript())
	}
}

func TestStreamEndingWithoutATerminalEventStopsTheRunWithANotice(t *testing.T) {
	// Arrange
	m, _ := newModel(&fakeBackend{})
	ch := wire(t, text("partial"))
	m.Update(runStartedMsg{runID: "r1", events: ch})

	// Act
	drain(m, "r1", ch)

	// Assert
	tr := m.Transcript()
	if m.Running() || !strings.Contains(tr, "partial") || !strings.Contains(tr, "the event stream ended without a result") {
		t.Fatalf("running %v:\n%s", m.Running(), tr)
	}
}

func TestStreamEndOfAnOldRunDoesNotStopTheCurrentOne(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	m.Update(runStartedMsg{runID: "r2", events: make(chan client.Event)})

	m.Update(streamDoneMsg{run: "r1"})

	if !m.Running() {
		t.Fatal("the end of run r1's stream stopped run r2")
	}
}

func TestRunStartFailureIsShownAndTheModelAcceptsANewMessage(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	m.starting = true

	m.Update(runStartedMsg{err: errors.New("no model")})

	if m.starting || m.Running() || !strings.Contains(m.Transcript(), "no model") {
		t.Fatalf("starting %v running %v:\n%s", m.starting, m.Running(), m.Transcript())
	}
}

func TestContextCompactionIsAnnouncedOnlyWhenCompleted(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	startRun(t, m, "go")

	feed(t, m, agui.CustomEvent(agui.CustomCompaction, map[string]any{"phase": "started"}))
	started := m.Transcript()
	feed(t, m, agui.CustomEvent(agui.CustomCompaction, map[string]any{"phase": "completed"}))

	if strings.Contains(started, "compacted") || !strings.Contains(m.Transcript(), "context compacted") {
		t.Fatalf("started:\n%s\nafter completed:\n%s", started, m.Transcript())
	}
}

func TestTextWithoutAMessageStartOpensAnAssistantBlock(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	startRun(t, m, "go")

	feed(t, m, text("no start event"), text(" came before"))

	if tr := m.Transcript(); !strings.Contains(tr, "no start event came before") {
		t.Fatalf("transcript %q", tr)
	}
}

func TestRunsStateIsReportedInTheStatusLine(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	states := []string{m.statusView().State}

	typeText(m, "go")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	states = append(states, m.statusView().State) // send in flight
	for _, r := range msgs(cmd) {
		update(m, r)
	}
	states = append(states, m.statusView().State)
	press(m, tea.KeyEsc)
	states = append(states, m.statusView().State)
	feed(t, m, agui.RunFinishedEvent("t", "run1", "cancelled", nil))
	startRun(t, m, "again")
	feed(t, m, approvalInterrupt())
	states = append(states, m.statusView().State)

	want := []string{"idle", "starting", "running", "cancelling", "waiting"}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Fatalf("states %v, want %v", states, want)
	}
}
