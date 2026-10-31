package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestShellFailureIsShownAndSentToTheModelAsFailedContext(t *testing.T) {
	// Arrange
	be := &fakeBackend{}
	m, _ := newWS(t, be, nil)

	// Act
	update(m, shellDoneMsg{cmd: "make", out: "no rule\n", err: fmt.Errorf("exit status 2")})
	startRun(t, m, "why?")

	// Assert
	if tr := m.Transcript(); !strings.Contains(tr, "no rule") || !strings.Contains(tr, "[exit status 2]") {
		t.Fatalf("transcript:\n%s", tr)
	}
	sent := lastSend(be)
	if !strings.HasPrefix(sent, "I ran `make` and it failed (exit status 2); output:") || !strings.HasSuffix(sent, "why?") {
		t.Fatalf("sent %q", sent)
	}
}

func TestShellWithNoOutputSaysSoAndLongOutputIsFolded(t *testing.T) {
	m, _ := newWS(t, &fakeBackend{}, nil)
	var long strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&long, "row %d\n", i)
	}

	update(m, shellDoneMsg{cmd: "true"})
	update(m, shellDoneMsg{cmd: "seq", out: long.String()})

	tr := m.Transcript()
	if !strings.Contains(tr, "(no output)") || !strings.Contains(tr, "row 29") || strings.Contains(tr, "row 30") || !strings.Contains(tr, "… +10 lines") {
		t.Fatalf("transcript:\n%s", tr)
	}
}

func TestBangWithNothingAfterItDoesNothing(t *testing.T) {
	be := &fakeBackend{}
	m, _ := newWS(t, be, nil)

	typeText(m, "!   ")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if cmd != nil && len(msgs(cmd)) != 0 || len(be.Calls()) != 0 || len(m.blocks) != 0 {
		t.Fatalf("calls %v blocks %d", be.Calls(), len(m.blocks))
	}
}

func TestBangRunsInTheWorkspaceAndReportsAFailingExit(t *testing.T) {
	m, root := newWS(t, &fakeBackend{}, nil)

	typeText(m, "!pwd; exit 3")
	press(m, tea.KeyEnter)

	tr := m.Transcript()
	if !strings.Contains(tr, "$ pwd; exit 3") || !strings.Contains(tr, "exit status 3") || !strings.Contains(tr, strings.TrimPrefix(root, "/private")) {
		t.Fatalf("transcript:\n%s", tr)
	}
}
