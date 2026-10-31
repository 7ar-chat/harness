package sandboxtools_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

func TestEditFileRejectsBadArguments(t *testing.T) {
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/a.txt", "hello") })

	// Act
	_, empty := f.call(context.Background(), "edit_file", `{"path":"a.txt","oldText":"","newText":"x"}`)
	_, noPath := f.call(context.Background(), "edit_file", `{"path":"","oldText":"a","newText":"x"}`)
	_, missing := f.call(context.Background(), "edit_file", `{"path":"nope.txt","oldText":"a","newText":"x"}`)

	// Assert
	if empty == nil || !strings.Contains(empty.Error(), "oldText must not be empty") {
		t.Errorf("empty oldText: %v", empty)
	}
	if noPath == nil || !strings.Contains(noPath.Error(), "path is required") {
		t.Errorf("no path: %v", noPath)
	}
	if missing == nil || !strings.Contains(missing.Error(), "/workspace/nope.txt: no such file or directory") {
		t.Errorf("missing file: %v", missing)
	}
	if got, _ := f.sb.File("/workspace/a.txt"); got != "hello" {
		t.Errorf("file changed: %q", got)
	}
}

func TestEditFileRefusesAFileTooLargeToEditWithoutTouchingIt(t *testing.T) {
	// Arrange: a file over the 5 MiB read cap, which read_file only shows in part and so never counts as read.
	big := strings.Repeat("0123456789\n", 500000)
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/big.log", big) })
	_, _ = f.call(context.Background(), "read_file", `{"path":"big.log","limit":1}`)

	// Act
	_, err := f.call(context.Background(), "edit_file", `{"path":"big.log","oldText":"0123456789","newText":"x","replaceAll":true}`)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "too large to edit") || !strings.Contains(err.Error(), "execute_command") {
		t.Fatalf("err %v", err)
	}
	if got, _ := f.sb.File("/workspace/big.log"); got != big {
		t.Error("the file was modified")
	}
}

func TestEditFileOfAnUnreadNonUTF8FileDoesNotShowItsBytes(t *testing.T) {
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/latin1.txt", "caf\xe9 au lait") })

	// Act
	_, err := f.call(context.Background(), "edit_file", `{"path":"latin1.txt","oldText":"au","newText":"du"}`)

	// Assert: no "current content below" dump of invalid bytes; the model is sent to read_file.
	if err == nil || !strings.Contains(err.Error(), "read it with read_file before editing it") || strings.Contains(err.Error(), "au lait") {
		t.Fatalf("err %v", err)
	}
	if got, _ := f.sb.File("/workspace/latin1.txt"); got != "caf\xe9 au lait" {
		t.Errorf("file changed: %q", got)
	}
}

func TestEditFileRefusalShowsTheFileAndTheRetryNeedsNoRead(t *testing.T) {
	// Arrange: a short file with one overlong line.
	long := strings.Repeat("x", 2500)
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/a.txt", "first\n"+long+"\nlast\n") })

	// Act
	_, refused := f.call(context.Background(), "edit_file", `{"path":"a.txt","oldText":"last","newText":"end"}`)
	_, retried := f.call(context.Background(), "edit_file", `{"path":"a.txt","oldText":"last","newText":"end"}`)

	// Assert
	if refused == nil || !strings.Contains(refused.Error(), "     1\tfirst\n") || !strings.Contains(refused.Error(), "     3\tlast") ||
		!strings.Contains(refused.Error(), "[line truncated]") || strings.Contains(refused.Error(), long) {
		t.Fatalf("refusal %v", refused)
	}
	if got, _ := f.sb.File("/workspace/a.txt"); retried != nil || !strings.HasSuffix(got, "\nend\n") {
		t.Fatalf("retry err %v file tail %q", retried, got[max(0, len(got)-20):])
	}
}

func TestEditFileRefusalOfABigFileShowsOnlyTheRegionsClosestToOldText(t *testing.T) {
	// Arrange: a file far over one read_file page.
	var b strings.Builder
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&b, "filler line number %d\n", i)
	}
	b.WriteString("func targetFunction(arg int) error {\n")
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/big.go", b.String()) })

	// Act
	_, err := f.call(context.Background(), "edit_file", `{"path":"big.go","oldText":"func targetFunction(arg int) error {","newText":"x"}`)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "5001 lines; the regions closest to oldText") ||
		!strings.Contains(err.Error(), "  5001\tfunc targetFunction(arg int) error {") || strings.Contains(err.Error(), "filler line number 2000") {
		t.Fatalf("err %.600v", err)
	}
}

func TestEditFileKeepsCRLFLineEndingsOnAToleratedMatch(t *testing.T) {
	// Arrange
	file := "func a() {\r\n\treturn 1\r\n}\r\n"
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/a.go", file) })

	// Act
	out, err := readThenEdit(t, f, "a.go", `{"path":"a.go","oldText":"    return 1","newText":"    return 2"}`)

	// Assert: the match ignores the indentation kind, the file keeps its tabs and CRLFs.
	got, _ := f.sb.File("/workspace/a.go")
	if err != nil || !strings.Contains(out, `"matched":"whitespace"`) || got != "func a() {\r\n\treturn 2\r\n}\r\n" {
		t.Fatalf("err %v out %s file %q", err, out, got)
	}
}

func TestEditFileToleratedMatchWithEmptyNewTextDeletesTheLines(t *testing.T) {
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/a.py", "keep\n    drop me\nkeep too\n") })

	// Act
	out, err := readThenEdit(t, f, "a.py", `{"path":"a.py","oldText":"  drop me  ","newText":""}`)

	// Assert: the whole line goes, no blank line is left behind.
	got, _ := f.sb.File("/workspace/a.py")
	if err != nil || !strings.Contains(out, `"matched":"whitespace"`) || got != "keep\nkeep too\n" {
		t.Fatalf("err %v out %s file %q", err, out, got)
	}
}

func TestEditFileReplaceAllNeedsAnExactMatch(t *testing.T) {
	// Arrange
	file := "x = 1\n    y = 2\n"
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/a.py", file) })

	// Act
	_, err := readThenEdit(t, f, "a.py", `{"path":"a.py","oldText":"y = 2 ","newText":"y = 3","replaceAll":true}`)

	// Assert: replaceAll never falls back to the whitespace-tolerant match.
	if got, _ := f.sb.File("/workspace/a.py"); err == nil || !strings.Contains(err.Error(), "replaceAll needs an exact match") || got != file {
		t.Fatalf("err %v file %q", err, got)
	}
}

func TestEditFileMissWithNothingSimilarSaysSo(t *testing.T) {
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/a.txt", "alpha\nbeta\n") })

	// Act
	_, none := readThenEdit(t, f, "a.txt", `{"path":"a.txt","oldText":"zzzzqqqq","newText":"x"}`)
	_, blank := f.call(context.Background(), "edit_file", `{"path":"a.txt","oldText":"\n\n","newText":"x"}`)
	_, longer := f.call(context.Background(), "edit_file", `{"path":"a.txt","oldText":"alpha\nbeta\ngamma\ndelta","newText":"x"}`)

	// Assert
	if none == nil || !strings.Contains(none.Error(), "No similar lines found") {
		t.Errorf("none: %v", none)
	}
	if blank == nil || !strings.Contains(blank.Error(), "Read the file and copy the exact text") {
		t.Errorf("blank: %v", blank)
	}
	if longer == nil || !strings.Contains(longer.Error(), "not found") {
		t.Errorf("longer than the file: %v", longer)
	}
}

func TestEditFileMissListsAtMostThreeNonOverlappingRegions(t *testing.T) {
	// Arrange: five similar lines, adjacent ones overlap and count once.
	file := "value total_a = 1\nvalue total_b = 2\nunrelated\n" + strings.Repeat("filler\n", 10) +
		"value total_c = 3\n" + strings.Repeat("filler\n", 10) + "value total_d = 4\n" + strings.Repeat("filler\n", 10) + "value total_e = 5\n"
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/a.txt", file) })

	// Act
	_, err := readThenEdit(t, f, "a.txt", `{"path":"a.txt","oldText":"value total_x = 9","newText":"y"}`)

	// Assert
	if err == nil || strings.Count(err.Error(), "--- lines") != 3 {
		t.Fatalf("err %v", err)
	}
}

func TestEditFileForgetsSomeFileOfAThreadBeyondItsReadBudget(t *testing.T) {
	// The memory of read files is bounded (500 per thread): after reading 501 files, one of them needs a new read before it can be edited.
	// Arrange
	const n = 501
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		for i := 0; i < n; i++ {
			sb.Put(fmt.Sprintf("/workspace/f%03d.txt", i), "one")
		}
	})
	for i := 0; i < n; i++ {
		if _, err := f.call(context.Background(), "read_file", fmt.Sprintf(`{"path":"f%03d.txt"}`, i)); err != nil {
			t.Fatal(err)
		}
	}

	// Act: edit until the first refusal (an edit of a remembered file forgets nothing).
	var refused error
	edited := 0
	for i := 0; i < n && refused == nil; i++ {
		if _, err := f.call(context.Background(), "edit_file", fmt.Sprintf(`{"path":"f%03d.txt","oldText":"one","newText":"two"}`, i)); err != nil {
			refused = err
			continue
		}
		edited++
	}

	// Assert
	if refused == nil || !strings.Contains(refused.Error(), "you have not read") {
		t.Fatalf("edited %d, refusal %v", edited, refused)
	}
}
