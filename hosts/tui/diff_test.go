package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

const editArgs = `{"path":"a.go","oldText":"func a() int {\n\treturn 1\n}","newText":"func a() int {\n\tx := compute()\n\treturn x + 2\n}"}`

func editedAt(t *testing.T, cols int, cfg string) *Model {
	t.Helper()
	c := ParseTUIConfig([]byte(cfg))
	m := New(&fakeBackend{}, Options{Now: func() time.Time { return t0 }, StaticCursor: true, Config: &c})
	m.Update(tea.WindowSizeMsg{Width: cols, Height: 30})
	startRun(t, m, "fix it")
	feed(t, m, toolEvents("c1", "edit_file", editArgs, `{"path":"a.go","replacements":1,"matched":"exact"}`, false)...)
	return m
}

func TestWideTerminalShowsTheEditSideBySide(t *testing.T) {
	// Arrange
	m := editedAt(t, 120, "")

	// Act
	got := m.Transcript()

	// Assert
	golden(t, "edit_diff_split", got)
}

func TestSplitDiffWrapsLongLinesInsideTheirColumn(t *testing.T) {
	// Arrange
	long := strings.Repeat("word ", 30)
	diff := RenderDiff("a.go", "keep\nold "+long, "keep\nnew "+long)

	// Act
	lines := renderSplitDiff(diff, 120, 80)

	// Assert
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > 118 {
			t.Fatalf("line of %d columns: %q", w, l)
		}
	}
	if len(lines) < 6 {
		t.Fatalf("long line not wrapped:\n%s", strings.Join(lines, "\n"))
	}
	golden(t, "edit_diff_split_wrapped", strings.Join(lines, "\n"))
}

func TestStackedDiffStyleKeepsOneColumnEvenWhenWide(t *testing.T) {
	m := editedAt(t, 160, `{"diff_style": "stacked"}`)

	got := m.Transcript()

	if strings.Contains(got, "│") {
		t.Fatalf("split view in stacked mode:\n%s", got)
	}
}

func TestNarrowTerminalStaysStackedInAuto(t *testing.T) {
	m := editedAt(t, 119, "")

	if got := m.Transcript(); strings.Contains(got, " │ ") {
		t.Fatalf("split view under 120 columns:\n%s", got)
	}
}

func TestDiffCommandTogglesTheViewAtRuntime(t *testing.T) {
	m := editedAt(t, 120, "")
	if !strings.Contains(m.Transcript(), " │ ") {
		t.Fatal("not split to begin with")
	}

	typeText(m, "/diff")
	press(m, tea.KeyEnter)
	stacked := m.Transcript()
	typeText(m, "/diff")
	press(m, tea.KeyEnter)

	if strings.Contains(stacked, " │ ") {
		t.Fatalf("still split after /diff:\n%s", stacked)
	}
	if !strings.Contains(m.Transcript(), " │ ") {
		t.Fatal("not split again after the second /diff")
	}
}

func TestSplitRowsPairRemovedWithAddedLines(t *testing.T) {
	rows := splitRows([]string{" ctx", "-a", "-b", "+c", " end"})

	if len(rows) != 4 {
		t.Fatalf("%d rows: %+v", len(rows), rows)
	}
	if rows[1].oldText != "a" || rows[1].newText != "c" || rows[2].oldText != "b" || rows[2].newNo != 0 {
		t.Fatalf("rows %+v", rows)
	}
	if rows[3].oldNo != 4 || rows[3].newNo != 3 {
		t.Fatalf("numbers after the change: %+v", rows[3])
	}
}
