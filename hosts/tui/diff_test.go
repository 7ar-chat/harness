package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
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
	m := editedAt(t, 110, "")

	// Act
	got := m.Transcript()

	// Assert
	golden(t, "edit_diff_split", got)
}

func TestSplitDiffFallsBackToStackedForLongLines(t *testing.T) {
	// Arrange
	long := strings.Repeat("word ", 30)
	diff := RenderDiff("a.go", "keep\nold "+long, "keep\nnew "+long)

	// Act
	lines := renderSplitDiff(diff, 120, 80)

	// Assert: a line that would wrap inside its half sends the diff to the stacked view
	if lines != nil {
		t.Fatalf("split view wraps long lines:\n%s", strings.Join(lines, "\n"))
	}
	for _, l := range stackedDiff(strings.Split(diff, "\n"), 120, 80) {
		if w := ansi.StringWidth(l); w > 120 {
			t.Fatalf("stacked line of %d columns: %q", w, l)
		}
	}
}

func TestStackedDiffStyleKeepsOneColumnEvenWhenWide(t *testing.T) {
	m := editedAt(t, 160, `{"diff_style": "stacked"}`)

	got := m.Transcript()

	if strings.Contains(got, "│") {
		t.Fatalf("split view in stacked mode:\n%s", got)
	}
}

func TestNarrowTerminalStaysStackedInAuto(t *testing.T) {
	m := editedAt(t, 109, "")

	if got := m.Transcript(); strings.Contains(got, " │ ") {
		t.Fatalf("split view under 110 columns:\n%s", got)
	}
}

func TestDiffCommandTogglesTheViewAtRuntime(t *testing.T) {
	m := editedAt(t, 110, "")
	if !strings.Contains(m.Transcript(), " │ ") {
		t.Fatal("not split to begin with")
	}

	typeText(m, "/diff")
	press(m, kEnter)
	stacked := m.Transcript()
	typeText(m, "/diff")
	press(m, kEnter)

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

func TestDiffNumbersFollowTheStartLineOfTheEdit(t *testing.T) {
	tv := ToolView{Name: "edit_file", Done: true,
		Args:   `{"path":"a.go","oldText":"x\ny","newText":"x\nz"}`,
		Result: `{"path":"a.go","replacements":1,"startLine":41}`}
	got := renderToolCall(tv, 80, false, false)
	for _, want := range []string{"+1 −1", "41  x", "42 -y", "42 +z"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	split := strings.Join(renderSplitDiff(editDiff(tv), 130, 20), "\n")
	if !strings.Contains(split, "42 - y") || !strings.Contains(split, "42 + z") {
		t.Errorf("split view not numbered from 41:\n%s", split)
	}
}
