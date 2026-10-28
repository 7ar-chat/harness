package sandboxtools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/tools/sandboxtools"
)

// readThenEdit reads the file (edit_file requires it) then edits it.
func readThenEdit(t *testing.T, f *fixture, path, args string) (string, error) {
	t.Helper()
	if _, err := f.call(context.Background(), "read_file", `{"path":"`+path+`"}`); err != nil {
		t.Fatal(err)
	}
	return f.call(context.Background(), "edit_file", args)
}

func TestEditFileReplacesUniqueMatch(t *testing.T) {
	// edit_file replaces exactly one exact occurrence of a file the model has read and reports it.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/a.py", "x = 1\ny = 2\n") })

	// Act
	out, err := readThenEdit(t, f, "a.py", `{"path":"a.py","oldText":"y = 2","newText":"y = 3"}`)

	// Assert
	got, _ := f.sb.File("/workspace/a.py")
	if err != nil || out != `{"path":"/workspace/a.py","replacements":1,"matched":"exact"}` || got != "x = 1\ny = 3\n" {
		t.Fatalf("err %v out %s file %q", err, out, got)
	}
}

func TestEditFileRejectsAmbiguousText(t *testing.T) {
	// Several exact matches without replaceAll are an error that leaves the file untouched.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/a.txt", "a a a") })

	// Act
	_, err := readThenEdit(t, f, "a.txt", `{"path":"a.txt","oldText":"a","newText":"b"}`)

	// Assert
	got, _ := f.sb.File("/workspace/a.txt")
	if err == nil || !strings.Contains(err.Error(), "appears 3 times") || got != "a a a" {
		t.Fatalf("err %v file %q", err, got)
	}
}

func TestEditFileReplaceAll(t *testing.T) {
	// replaceAll replaces every exact occurrence.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/a.txt", "a a a") })

	// Act
	out, err := readThenEdit(t, f, "a.txt", `{"path":"a.txt","oldText":"a","newText":"b","replaceAll":true}`)

	// Assert
	got, _ := f.sb.File("/workspace/a.txt")
	if err != nil || !strings.Contains(out, `"replacements":3`) || got != "b b b" {
		t.Fatalf("err %v out %s file %q", err, out, got)
	}
}

func TestEditFileToleratesIndentationWhenTheMatchIsUnique(t *testing.T) {
	// When oldText differs from the file only by indentation and trailing spaces, the unique matching lines are replaced and the new text takes the file's indentation.
	// Arrange
	file := "def f():\n    if x:   \n        return 1\n    return 2\n"
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/a.py", file) })

	// Act
	out, err := readThenEdit(t, f, "a.py", `{"path":"a.py","oldText":"if x:\n    return 1","newText":"if x:\n    return 10"}`)

	// Assert
	got, _ := f.sb.File("/workspace/a.py")
	if err != nil || !strings.Contains(out, `"matched":"whitespace"`) || got != "def f():\n    if x:\n        return 10\n    return 2\n" {
		t.Fatalf("err %v out %s file %q", err, out, got)
	}
}

func TestEditFileRefusesAToleratedMatchThatIsNotUnique(t *testing.T) {
	// A whitespace-tolerant match found in two places is refused (never guessed).
	// Arrange
	file := "  total = 1\nx\n    total = 1\n"
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/a.py", file) })

	// Act
	_, err := readThenEdit(t, f, "a.py", `{"path":"a.py","oldText":"total = 1 ","newText":"total = 2"}`)

	// Assert
	got, _ := f.sb.File("/workspace/a.py")
	if err == nil || !strings.Contains(err.Error(), "matches 2 places") || got != file {
		t.Fatalf("err %v file %q", err, got)
	}
}

func TestEditFileMissShowsTheClosestLinesWithNumbers(t *testing.T) {
	// When oldText is not found, the error shows the most similar region with its line numbers so the model can copy the exact text.
	// Arrange
	file := "# Report\n\nRevenue grew by 12% in Q3.\nCosts were flat.\n"
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/r.md", file) })

	// Act
	_, err := readThenEdit(t, f, "r.md", `{"path":"r.md","oldText":"Revenue grew by 15% in Q3.","newText":"x"}`)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "lines 3-3") ||
		!strings.Contains(err.Error(), "     3\tRevenue grew by 12% in Q3.") {
		t.Fatalf("err %v", err)
	}
}

func TestEditFileRequiresAReadFirst(t *testing.T) {
	// A file the model never read nor wrote is not edited (the refusal shows it, round 3); reading it unlocks the edit.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/a.txt", "hello") })

	// Act
	_, refused := f.call(context.Background(), "edit_file", `{"path":"a.txt","oldText":"hello","newText":"bye"}`)
	_, err := readThenEdit(t, f, "a.txt", `{"path":"a.txt","oldText":"hello","newText":"bye"}`)

	// Assert
	got, _ := f.sb.File("/workspace/a.txt")
	if refused == nil || !strings.Contains(refused.Error(), "you have not read /workspace/a.txt in this conversation") || err != nil || got != "bye" {
		t.Fatalf("refused %v err %v file %q", refused, err, got)
	}
}

func TestEditFileRefusesAFileChangedSinceItWasRead(t *testing.T) {
	// A file changed by a command after the model read it is not edited from the stale view (no duplicated section); after a new read the edit applies.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/r.md", "# Report\n## Summary\n") })
	if _, err := f.call(context.Background(), "read_file", `{"path":"r.md"}`); err != nil {
		t.Fatal(err)
	}
	f.sb.Put("/workspace/r.md", "# Report\n## Summary\n## Risks\n") // a script appended the section meanwhile
	edit := `{"path":"r.md","oldText":"## Summary\n","newText":"## Summary\n## Risks\n"}`

	// Act
	_, stale := f.call(context.Background(), "edit_file", edit)

	// Assert
	got, _ := f.sb.File("/workspace/r.md")
	if stale == nil || !strings.Contains(stale.Error(), sandboxtools.StaleFileError) || strings.Count(got, "## Risks") != 1 {
		t.Fatalf("stale %v file %q", stale, got)
	}
}

func TestEditFileAcceptsAFileTheModelWrote(t *testing.T) {
	// write_file counts as having seen the file: consecutive write and edits need no extra read.
	// Arrange
	f := newFixture(t, nil)
	if _, err := f.call(context.Background(), "write_file", `{"path":"n.md","content":"one\n"}`); err != nil {
		t.Fatal(err)
	}

	// Act
	_, err1 := f.call(context.Background(), "edit_file", `{"path":"n.md","oldText":"one","newText":"two"}`)
	_, err2 := f.call(context.Background(), "edit_file", `{"path":"n.md","oldText":"two","newText":"three"}`)

	// Assert
	got, _ := f.sb.File("/workspace/n.md")
	if err1 != nil || err2 != nil || got != "three\n" {
		t.Fatalf("err %v %v file %q", err1, err2, got)
	}
}
