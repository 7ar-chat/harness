package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestTimelineListsYourMessagesAndJumpsToTheChosenOne(t *testing.T) {
	// Arrange
	m, _ := newModel(&fakeBackend{})
	longTranscript(m)

	// Act
	typeText(m, "/timeline")
	press(m, tea.KeyEnter)
	view := m.View()
	for i := 0; i < 9; i++ {
		press(m, tea.KeyUp) // question 19 -> 10
	}
	press(m, tea.KeyEnter)

	// Assert
	if !strings.Contains(view, "Timeline") || !strings.Contains(view, "question 19") || strings.Contains(view, "answer 19") {
		t.Fatalf("picker:\n%s", view)
	}
	offs := m.blockOffsets()
	if want := offs[2*10]; m.vp.YOffset != want || m.inp.sel != 2*10 {
		t.Fatalf("offset %d (want %d), selected block %d", m.vp.YOffset, want, m.inp.sel)
	}
}

func TestTimelineHasALeaderKey(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	longTranscript(m)

	update(m, tea.KeyMsg{Type: tea.KeyCtrlX})
	typeText(m, "g")

	if m.picker == nil || m.picker.title != "Timeline" {
		t.Fatal("ctrl+x g did not open the timeline")
	}
}

func TestTimelineWithoutMessagesSaysSo(t *testing.T) {
	m, _ := newModel(&fakeBackend{})

	typeText(m, "/timeline")
	press(m, tea.KeyEnter)

	if m.picker != nil || !strings.Contains(m.Transcript(), "no messages of yours") {
		t.Fatalf("picker %v\n%s", m.picker, m.Transcript())
	}
}

func TestMessageJumpActionsAreUnboundByDefaultAndRebindable(t *testing.T) {
	// Arrange
	m, _ := newConfigured(t, `{"keybinds": {"messages_previous": "ctrl+k", "messages_next": "ctrl+l", "messages_last_user": "ctrl+y"}}`, "")
	longTranscript(m)
	d, _ := newModel(&fakeBackend{})
	longTranscript(d)
	offs := m.blockOffsets()

	// Act
	update(m, tea.KeyMsg{Type: tea.KeyCtrlK})
	prev := m.vp.YOffset
	update(m, tea.KeyMsg{Type: tea.KeyCtrlK})
	prev2 := m.vp.YOffset
	update(m, tea.KeyMsg{Type: tea.KeyCtrlL})
	next := m.vp.YOffset
	m.vp.GotoTop()
	update(m, tea.KeyMsg{Type: tea.KeyCtrlY})
	last := m.vp.YOffset
	update(d, tea.KeyMsg{Type: tea.KeyCtrlK})

	// Assert
	if prev != offs[30] || prev2 != offs[28] || next != offs[30] {
		t.Fatalf("previous %d, previous %d, next %d; user message offsets %v", prev, prev2, next, offs)
	}
	if last != m.vp.TotalLineCount()-m.vp.Height || m.inp.sel != 38 { // the last message is within the final page
		t.Fatalf("last user message: offset %d, selected block %d", last, m.inp.sel)
	}
	if d.vp.YOffset != d.vp.TotalLineCount()-d.vp.Height {
		t.Fatalf("ctrl+k moved the view without a binding: %d", d.vp.YOffset)
	}
}

func TestBlockOffsetsAgreeWithBlockOffset(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	longTranscript(m)

	offs := m.blockOffsets()

	for i := range offs {
		if offs[i] != m.blockOffset(i) {
			t.Fatalf("block %d: %d vs %d", i, offs[i], m.blockOffset(i))
		}
	}
}
