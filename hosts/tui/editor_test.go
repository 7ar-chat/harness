package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestEditorCommandPrefersVisualOverEditorAndIsNilWhenBothAreUnset(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "  ")
	none := editorCommand()
	t.Setenv("EDITOR", "vi")
	editorOnly := editorCommand()
	t.Setenv("VISUAL", "code --wait")

	both := editorCommand()

	if none != nil || strings.Join(editorOnly, " ") != "vi" || strings.Join(both, " ") != "code --wait" {
		t.Fatalf("none %v editorOnly %v both %v", none, editorOnly, both)
	}
}

func TestEditorCommandWithoutAnEditorOffersTheDraftAndReportsTheMissingEditor(t *testing.T) {
	// Arrange
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	m, _ := newModel(&fakeBackend{})
	typeText(m, "half written")
	m.in.SetValue("/editor") // a slash command replaces the input; the draft is what was there before
	m.inp.draft = "half written"

	// Act
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	var done editorDoneMsg
	for _, r := range msgs(cmd) {
		if d, ok := r.(editorDoneMsg); ok {
			done = d
		}
	}
	data, err := os.ReadFile(done.path)
	update(m, done)

	// Assert
	if err != nil || string(data) != "half written" {
		t.Fatalf("temp file %q: %q (%v)", done.path, data, err)
	}
	if done.err == nil || !strings.Contains(m.Transcript(), "set $EDITOR (or $VISUAL) first") {
		t.Fatalf("err %v:\n%s", done.err, m.Transcript())
	}
	if _, err := os.Stat(done.path); !os.IsNotExist(err) {
		t.Fatalf("the temp file was left behind: %v", err)
	}
	if m.in.Value() != "" {
		t.Fatalf("a failed editor replaced the input with %q", m.in.Value())
	}
}

func TestEditorFailureAndUnreadableFileKeepTheInputAndReport(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	m.in.SetValue("keep me")
	f := filepath.Join(t.TempDir(), "m.md")
	os.WriteFile(f, []byte("changed"), 0o644)

	update(m, editorDoneMsg{path: f, err: os.ErrPermission})
	update(m, editorDoneMsg{path: filepath.Join(t.TempDir(), "gone.md")})

	tr := m.Transcript()
	if m.in.Value() != "keep me" || strings.Count(tr, "editor: ") != 2 {
		t.Fatalf("input %q:\n%s", m.in.Value(), tr)
	}
}

func TestExportWritesTheConversationAndSaysWhere(t *testing.T) {
	// Arrange
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	m, _ := newModel(&fakeBackend{})
	m.add(block{kind: blockUser, text: "fix the build"})
	m.add(block{kind: blockAssistant, text: "done"})

	// Act
	typeText(m, "/export")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	// Assert
	if cmd != nil && len(msgs(cmd)) != 0 {
		t.Fatalf("without an editor nothing should be opened")
	}
	tr := m.Transcript()
	i := strings.Index(tr, "exported to ")
	if i < 0 {
		t.Fatalf("no confirmation:\n%s", tr)
	}
	path := strings.Fields(tr[i+len("exported to "):])[0]
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "# fix the build") || !strings.Contains(string(data), "## Assistant\n\ndone") {
		t.Fatalf("export file %s: %q (%v)", path, data, err)
	}
}

func TestExportWithAnEditorOpensIt(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("EDITOR", "true")
	t.Setenv("VISUAL", "")
	m, _ := newModel(&fakeBackend{})
	m.add(block{kind: blockUser, text: "hi"})

	typeText(m, "/export")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if cmd == nil {
		t.Fatal("no editor command was returned")
	}
	update(m, editorDoneMsg{path: "x", export: true, err: os.ErrClosed})
	if !strings.Contains(m.Transcript(), "editor: ") {
		t.Fatalf("an editor failure on an export is not reported:\n%s", m.Transcript())
	}
}

func TestExportEditorExitDoesNotLoadTheFileIntoTheInput(t *testing.T) {
	m, _ := newModel(&fakeBackend{})
	f := filepath.Join(t.TempDir(), "export.md")
	os.WriteFile(f, []byte("# exported"), 0o644)

	update(m, editorDoneMsg{path: f, export: true})

	if m.in.Value() != "" {
		t.Fatalf("input %q", m.in.Value())
	}
	if _, err := os.Stat(f); err != nil {
		t.Fatalf("the export was deleted: %v", err)
	}
}

func TestExportMarkdownUntitledErrorAndLongResults(t *testing.T) {
	blocks := []block{
		{kind: blockError, text: "boom\nsecond line"},
		{kind: blockTool, tool: ToolView{Name: "run", Result: strings.Repeat("x", exportResultChars+50) + "\n```"}},
	}

	got := exportMarkdown("", blocks, t0)

	if !strings.HasPrefix(got, "# Conversation\n") || !strings.Contains(got, "> **Error:** boom second line") {
		t.Fatalf("export:\n%s", got)
	}
	if !strings.Contains(got, "… (truncated)") || strings.Contains(got, "xxxx```") {
		t.Fatalf("long result not cut:\n%s", got)
	}
	if strings.Count(got, "```") != 2 {
		t.Fatalf("a fence inside a result must not close the block:\n%s", got)
	}
}
