package sandboxtools_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/coding/sandboxtools"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

func TestReadFileRejectsBadArgumentsBeforeTouchingTheSandbox(t *testing.T) {
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/a.txt", "x\n") })

	// Act
	_, blank := f.call(context.Background(), "read_file", `{"path":" "}`)
	_, pages := f.call(context.Background(), "read_file", `{"path":"a.txt","pages":"1-2"}`)
	_, sheet := f.call(context.Background(), "read_file", `{"path":"a.txt","sheet":"S"}`)

	// Assert
	if blank == nil || !strings.Contains(blank.Error(), "path is required") {
		t.Errorf("blank: %v", blank)
	}
	for _, err := range []error{pages, sheet} {
		if err == nil || !strings.Contains(err.Error(), "pages and sheet only apply to documents") {
			t.Errorf("pages/sheet on a text file: %v", err)
		}
	}
}

func TestReadFileOfAnEmptyFileSaysSoInsteadOfReturningNothing(t *testing.T) {
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/e.txt", "") })

	// Act
	out, err := f.call(context.Background(), "read_file", `{"path":"e.txt"}`)

	// Assert
	if err != nil || out != "[/workspace/e.txt is empty]" {
		t.Fatalf("err %v out %q", err, out)
	}
}

func TestReadFileBoundaryOffsets(t *testing.T) {
	// Arrange: three lines, with a trailing newline that is not a fourth line.
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/a.txt", "one\ntwo\nthree\n") })
	read := func(args string) (string, error) { return f.call(context.Background(), "read_file", args) }

	// Act
	last, errLast := read(`{"path":"a.txt","offset":3}`)
	_, past := read(`{"path":"a.txt","offset":4}`)
	negative, errNeg := read(`{"path":"a.txt","offset":-5,"limit":1}`)
	zeroLimit, errZero := read(`{"path":"a.txt","offset":2,"limit":-1}`)
	exact, errExact := read(`{"path":"a.txt","offset":2,"limit":2}`)

	// Assert
	if errLast != nil || last != "     3\tthree\n" {
		t.Errorf("last line: %q %v", last, errLast)
	}
	if past == nil || !strings.Contains(past.Error(), "offset 4 is past the end of /workspace/a.txt (3 lines)") {
		t.Errorf("past the end: %v", past)
	}
	if errNeg != nil || negative != "     1\tone\n[Showing lines 1-1 of 3. Use offset=2 to read more.]\n" {
		t.Errorf("negative offset: %q %v", negative, errNeg)
	}
	if errZero != nil || zeroLimit != "     2\ttwo\n     3\tthree\n" {
		t.Errorf("non-positive limit means the default: %q %v", zeroLimit, errZero)
	}
	if errExact != nil || exact != "     2\ttwo\n     3\tthree\n" {
		t.Errorf("a page ending on the last line has no footer: %q %v", exact, errExact)
	}
}

func TestReadFileCutsOverlongLinesOnARuneBoundary(t *testing.T) {
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/w.txt", strings.Repeat("é", 1500)+"\nshort\n") })

	// Act
	out, err := f.call(context.Background(), "read_file", `{"path":"w.txt"}`)

	// Assert
	line, _, _ := strings.Cut(out, "\n")
	if err != nil || !strings.HasSuffix(line, " [line truncated]") || strings.ContainsRune(line, '\uFFFD') ||
		strings.Count(line, "é") != 1000 || !strings.Contains(out, "     2\tshort\n") {
		t.Fatalf("err %v line has %d é", err, strings.Count(line, "é"))
	}
}

func TestReadFileBinaryDetectionLooksOnlyAtTheStart(t *testing.T) {
	// Arrange: a NUL after the first 8 KiB does not make a text file binary; one inside the first 8 KiB does.
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.Put("/workspace/late.txt", strings.Repeat("a", 9000)+"\x00tail")
		sb.Put("/workspace/early.txt", strings.Repeat("a", 8191)+"\x00"+"tail")
	})

	// Act
	late, lateErr := f.call(context.Background(), "read_file", `{"path":"late.txt"}`)
	_, earlyErr := f.call(context.Background(), "read_file", `{"path":"early.txt"}`)

	// Assert
	if lateErr != nil || !strings.HasPrefix(late, "     1\taaaa") {
		t.Errorf("late NUL: %v", lateErr)
	}
	if earlyErr == nil || !strings.Contains(earlyErr.Error(), "looks like a binary file (8196 bytes)") {
		t.Errorf("early NUL: %v", earlyErr)
	}
}

func TestReadFileOfAPartiallyReadFileDoesNotUnlockEditing(t *testing.T) {
	// Arrange: over the 5 MiB cap, so the model has seen only the start.
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/huge.txt", strings.Repeat("0123456789\n", 600000)) })
	_, _ = f.call(context.Background(), "read_file", `{"path":"huge.txt","limit":1}`)

	// Act
	_, err := f.call(context.Background(), "edit_file", `{"path":"huge.txt","oldText":"0123456789","newText":"x"}`)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "too large to edit") {
		t.Fatalf("err %v", err)
	}
}

func TestReadFileOfImagesReportsMissingAndOversizedFiles(t *testing.T) {
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/big.png", strings.Repeat("x", 5<<20+1)) })
	rec := &recorder{vision: true}

	// Act
	_, missing := f.call(withRec(rec), "read_file", `{"path":"nope.jpg"}`)
	_, big := f.call(withRec(rec), "read_file", `{"path":"big.png"}`)

	// Assert
	if missing == nil || !strings.Contains(missing.Error(), "/workspace/nope.jpg: no such file or directory") {
		t.Errorf("missing: %v", missing)
	}
	if big == nil || !strings.Contains(big.Error(), "images over 5 MiB cannot be shown") || len(rec.images) != 0 {
		t.Errorf("big: %v (images %d)", big, len(rec.images))
	}
}

func TestReadFileOfDocumentsReportsEachFailureMode(t *testing.T) {
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.PutDoc("/workspace/scan.docx", sandboxtest.Doc{Parts: []string{""}})
		sb.PutDoc("/workspace/one.pdf", sandboxtest.Doc{Parts: []string{"only page"}})
		sb.PutDoc("/workspace/deck.pptx", sandboxtest.Doc{Parts: []string{"A", "B"}})
		sb.Put("/workspace/broken.odt", "not a document")
	})
	read := func(args string) (string, error) { return f.call(context.Background(), "read_file", args) }

	// Act
	_, missing := read(`{"path":"gone.pdf"}`)
	scan, scanErr := read(`{"path":"scan.docx"}`)
	one, oneErr := read(`{"path":"one.pdf"}`)
	deck, deckErr := read(`{"path":"deck.pptx","pages":"2"}`)
	_, broken := read(`{"path":"broken.odt"}`)
	_, past := read(`{"path":"one.pdf","offset":99}`)

	// Assert
	if missing == nil || !strings.Contains(missing.Error(), "/workspace/gone.pdf: no such file or directory") {
		t.Errorf("missing: %v", missing)
	}
	if scanErr != nil || scan != "[document: DOCX; showing 1]\n[no text could be extracted (scanned pages or images only?); look at it with view_file]\n" {
		t.Errorf("scan: %q %v", scan, scanErr)
	}
	if oneErr != nil || !strings.HasPrefix(one, "[document: 1 page; showing pages 1]\n") {
		t.Errorf("single page is singular: %q %v", one, oneErr)
	}
	if deckErr != nil || !strings.HasPrefix(deck, "[document: 2 slides; showing slides 2]\n") || strings.Contains(deck, "     1\t## Slide 1") {
		t.Errorf("slides: %q %v", deck, deckErr)
	}
	if broken == nil || !strings.Contains(broken.Error(), "cannot extract text from /workspace/broken.odt") {
		t.Errorf("broken: %v", broken)
	}
	if past == nil || !strings.Contains(past.Error(), "offset 99 is past the end") {
		t.Errorf("past: %v", past)
	}
}

func TestReadFileOfADocumentReportsHowTheExtractionScriptFailed(t *testing.T) {
	cases := map[string]struct {
		script func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error)
		want   string
	}{
		"all pages":   {printing(`{"kind":"pdf","unit":"pages","total":4,"shown":"all","text":"body"}`+"\n", 0), "[document: 4 pages; showing all]"},
		"noise first": {printing("warning: slow font\n"+`{"kind":"pdf","unit":"pages","total":1,"shown":"1","text":"body"}`+"\n", 0), "[document: 1 page; showing pages 1]"},
		"no json":     {printing("Traceback...\n", 7), "sandbox script gave no result (exit 7)"},
		"exec error": {func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
			return sandbox.ExecResult{}, errors.New("boom")
		}, "sandbox script failed: boom"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			sb := newScripted(map[string]func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error){"# work:extract": c.script})
			sb.Put("/workspace/a.pdf", "%PDF")
			ts := toolsOn(t, sb, sandboxtools.Options{})

			// Act
			out, err := ts["read_file"].InvokableRun(context.Background(), `{"path":"a.pdf"}`)

			// Assert
			got := out
			if err != nil {
				got = err.Error()
			}
			if !strings.Contains(got, c.want) {
				t.Fatalf("out %q err %v, want %q", out, err, c.want)
			}
		})
	}
}

func TestReadFileOfADocumentStopsWhenTheCallIsCancelled(t *testing.T) {
	// Arrange
	ctx, cancel := context.WithCancel(context.Background())
	sb := newScripted(map[string]func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error){
		"# work:extract": func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
			cancel()
			return sandbox.ExecResult{}, context.Canceled
		}})
	sb.Put("/workspace/a.pdf", "%PDF")
	ts := toolsOn(t, sb, sandboxtools.Options{})

	// Act
	_, err := ts["read_file"].InvokableRun(ctx, `{"path":"a.pdf"}`)

	// Assert
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "sandbox script failed") {
		t.Fatalf("err %v", err)
	}
}

func TestReadFileOfADocumentRetriesOnceWhenTheSandboxIsFoundGone(t *testing.T) {
	// Arrange: the first extraction finds the sandbox gone, the retry succeeds.
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.PutDoc("/workspace/a.pdf", sandboxtest.Doc{Parts: []string{"hello"}})
	})
	if _, err := f.call(context.Background(), "list_files", `{}`); err != nil {
		t.Fatal(err)
	}
	f.sb.FailScript("# work:extract", sandbox.ErrGone)

	// Act
	out, err := f.call(context.Background(), "read_file", `{"path":"a.pdf"}`)

	// Assert
	if err != nil || !strings.Contains(out, "hello") {
		t.Fatalf("err %v out %q", err, out)
	}
}

func TestExtractText(t *testing.T) {
	// Arrange
	ctx := context.Background()
	sb := sandboxtest.NewSandbox("sbx")
	sb.PutDoc("/workspace/r.pdf", sandboxtest.Doc{Parts: []string{"alpha beta gamma"}})
	sb.PutDoc("/workspace/x.xlsx", sandboxtest.Doc{Names: []string{"S"}, Parts: []string{"| 1 |"}})
	sb.Put("/workspace/notes.txt", "héllo wörld, this is long")
	sb.Put("/workspace/blob.dat", "ab\x00cd")
	sb.Put("/workspace/chart.PNG", "\x89PNG")
	sb.Put("/workspace/bad.docx", "junk")

	// Act
	doc, docErr := sandboxtools.ExtractText(ctx, sb, "/workspace/r.pdf", 1000)
	cut, cutErr := sandboxtools.ExtractText(ctx, sb, "/workspace/r.pdf", 10)
	text, textErr := sandboxtools.ExtractText(ctx, sb, "/workspace/notes.txt", 1000)
	runes, runesErr := sandboxtools.ExtractText(ctx, sb, "/workspace/notes.txt", 5)
	binary, binaryErr := sandboxtools.ExtractText(ctx, sb, "/workspace/blob.dat", 1000)
	image, imageErr := sandboxtools.ExtractText(ctx, sb, "/workspace/chart.PNG", 1000)
	_, missing := sandboxtools.ExtractText(ctx, sb, "/workspace/none.txt", 10)
	_, bad := sandboxtools.ExtractText(ctx, sb, "/workspace/bad.docx", 10)
	sb.FailScript("# work:extract", errors.New("boom"))
	_, failed := sandboxtools.ExtractText(ctx, sb, "/workspace/x.xlsx", 10)

	// Assert
	if docErr != nil || doc != "## Page 1\nalpha beta gamma" {
		t.Errorf("document: %q %v", doc, docErr)
	}
	if cutErr != nil || cut != "## Page 1\n…" {
		t.Errorf("cut document: %q %v", cut, cutErr)
	}
	if textErr != nil || text != "héllo wörld, this is long" || runesErr != nil || runes != "héllo…" {
		t.Errorf("text: %q %v / %q %v", text, textErr, runes, runesErr)
	}
	if binaryErr != nil || binary != "" || imageErr != nil || image != "" {
		t.Errorf("binary %q %v image %q %v", binary, binaryErr, image, imageErr)
	}
	if !errors.Is(missing, sandbox.ErrNotFound) {
		t.Errorf("missing: %v", missing)
	}
	if bad == nil || !strings.Contains(bad.Error(), "cannot extract text from /workspace/bad.docx") {
		t.Errorf("unreadable document: %v", bad)
	}
	if failed == nil || !strings.Contains(failed.Error(), "boom") {
		t.Errorf("script failure: %v", failed)
	}
}
