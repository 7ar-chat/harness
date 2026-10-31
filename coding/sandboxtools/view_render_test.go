package sandboxtools_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/7ar-chat/harness/engine/tools"
)

// renderSoffice puts on PATH a soffice that behaves like LibreOffice for the
// render script: it logs one line per conversion, writes <outdir>/<name>.pdf
// with FAKE_PAGES blank pages (pypdf) and only exits FAKE_SLEEP seconds later
// (LibreOffice shuts down after writing). With FAKE_FAIL=always every
// conversion fails, with FAKE_FAIL=first only the first one: it prints the
// harmless javaldx warning on stderr and LibreOffice's load error on stdout,
// writes nothing and exits 1. The render script needs python3 with pypdf and
// pdftoppm: the test is skipped without them.
func renderSoffice(t *testing.T, extra map[string]string) (env map[string]string, log string) {
	t.Helper()
	if exec.Command("python3", "-c", "import pypdf").Run() != nil {
		t.Skip("python3 with pypdf is required")
	}
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		t.Skip("pdftoppm is required")
	}
	bin := t.TempDir()
	log = filepath.Join(bin, "soffice.log")
	script := `#!/usr/bin/env bash
echo "$*" >> "` + log + `"
out=.; src=""
while [ $# -gt 0 ]; do
  case "$1" in
    --convert-to) shift 2 ;;
    --outdir) out="$2"; shift 2 ;;
    -*) shift ;;
    *) src="$1"; shift ;;
  esac
done
if [ "$FAKE_FAIL" = always ] || { [ "$FAKE_FAIL" = first ] && [ ! -e "` + bin + `/failed" ]; }; then
  : > "` + bin + `/failed"
  echo "Warning: failed to launch javaldx - java may not function correctly" >&2
  echo "Error: source file could not be loaded"
  exit 1
fi
b=$(basename "$src")
python3 -c 'import sys; from pypdf import PdfWriter
w = PdfWriter()
for _ in range(int(sys.argv[2])): w.add_blank_page(200, 100)
w.write(sys.argv[1])' "$out/${b%.*}.pdf" "${FAKE_PAGES:-3}"
sleep "${FAKE_SLEEP:-0}"
`
	if err := os.WriteFile(filepath.Join(bin, "soffice"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	env = map[string]string{"PATH": bin + ":" + os.Getenv("PATH")}
	for k, v := range extra {
		env[k] = v
	}
	return env, log
}

func conversions(t *testing.T, log string) int {
	t.Helper()
	data, _ := os.ReadFile(log)
	if s := strings.TrimSpace(string(data)); s != "" {
		return len(strings.Split(s, "\n"))
	}
	return 0
}

func writeDeck(t *testing.T, f *fixture, name string) {
	t.Helper()
	ctx := tools.WithCall(context.Background(), "w", &recorder{vision: true})
	if _, err := f.call(ctx, "execute_command", `{"command":"mkdir -p outputs && printf deck > outputs/`+name+`"}`); err != nil {
		t.Fatal(err)
	}
}

func TestParallelViewsOfTheSameFileBothRender(t *testing.T) {
	// Two view_file calls of the same deck in one step (pages 1-2 and 3, run in parallel) both get their pages: the second waits for the first conversion and reuses its PDF instead of racing it in the same render directory.
	// Arrange
	env, log := renderSoffice(t, map[string]string{"FAKE_SLEEP": "1"})
	f, _ := newHostFixture(t, env)
	writeDeck(t, f, "deck.pptx")
	type res struct {
		out string
		err error
	}
	results := make([]res, 2)
	var wg sync.WaitGroup

	// Act
	for i, pages := range []string{"1-2", "3"} {
		wg.Add(1)
		go func(i int, pages string) {
			defer wg.Done()
			out, err := f.call(tools.WithCall(context.Background(), "v", &recorder{vision: true}), "view_file", `{"path":"outputs/deck.pptx","pages":"`+pages+`"}`)
			results[i] = res{out, err}
		}(i, pages)
	}
	wg.Wait()

	// Assert
	for i, want := range []int{2, 1} {
		var r viewResult
		_ = json.Unmarshal([]byte(results[i].out), &r)
		if results[i].err != nil || len(r.Pages) != want || r.TotalPages != 3 {
			t.Fatalf("view %d: err %v out %s", i, results[i].err, results[i].out)
		}
	}
	if n := conversions(t, log); n != 1 {
		t.Fatalf("%d conversions, want 1", n)
	}
}

func TestViewFileSaysWhyLibreOfficeCouldNotRender(t *testing.T) {
	// When LibreOffice writes no PDF, the error gives its exit code and its own message, not only the harmless javaldx warning.
	// Arrange
	env, _ := renderSoffice(t, map[string]string{"FAKE_FAIL": "always"})
	f, _ := newHostFixture(t, env)
	writeDeck(t, f, "deck.pptx")

	// Act
	_, err := f.call(tools.WithCall(context.Background(), "v", &recorder{vision: true}), "view_file", `{"path":"outputs/deck.pptx"}`)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "source file could not be loaded") || !strings.Contains(err.Error(), "exit 1") ||
		strings.Contains(err.Error(), "javaldx") {
		t.Fatalf("err %v", err)
	}
}

func TestViewFileRetriesAFailedConversionOnce(t *testing.T) {
	// A conversion that fails once (a transient LibreOffice start-up failure) is retried with a fresh profile and the pages are shown.
	// Arrange
	env, log := renderSoffice(t, map[string]string{"FAKE_FAIL": "first"})
	f, _ := newHostFixture(t, env)
	writeDeck(t, f, "deck.pptx")

	// Act
	out, err := f.call(tools.WithCall(context.Background(), "v", &recorder{vision: true}), "view_file", `{"path":"outputs/deck.pptx"}`)

	// Assert
	var r viewResult
	_ = json.Unmarshal([]byte(out), &r)
	if err != nil || len(r.Pages) != 3 || conversions(t, log) != 2 {
		t.Fatalf("err %v out %s conversions %d", err, out, conversions(t, log))
	}
}

func TestViewFileOfAPagePastTheEndListsTheValidPages(t *testing.T) {
	// Asking only for pages past the end is an error that says how many pages the document has and which pages are valid.
	// Arrange
	env, _ := renderSoffice(t, nil)
	f, _ := newHostFixture(t, env)
	writeDeck(t, f, "book.xlsx")

	// Act
	_, err := f.call(tools.WithCall(context.Background(), "v", &recorder{vision: true}), "view_file", `{"path":"outputs/book.xlsx","pages":"4"}`)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "past the end") || !strings.Contains(err.Error(), "3 pages") ||
		!strings.Contains(err.Error(), "valid pages: 1-3") {
		t.Fatalf("err %v", err)
	}
}

func TestViewFileShowsTheValidPagesAndNamesTheMissingOnes(t *testing.T) {
	// A range running past the end shows the pages that exist and its note names the ones that do not.
	// Arrange
	env, _ := renderSoffice(t, nil)
	f, _ := newHostFixture(t, env)
	writeDeck(t, f, "deck.pptx")

	// Act
	out, err := f.call(tools.WithCall(context.Background(), "v", &recorder{vision: true}), "view_file", `{"path":"outputs/deck.pptx","pages":"2-5"}`)

	// Assert
	var r viewResult
	_ = json.Unmarshal([]byte(out), &r)
	if err != nil || len(r.Pages) != 2 || r.Pages[0].Page != 2 || !strings.Contains(r.Note, "4-5") || !strings.Contains(r.Note, "3 slides") {
		t.Fatalf("err %v out %s", err, out)
	}
}

func TestReadFileOfPagesPastTheEndListsTheValidPages(t *testing.T) {
	// read_file of PDF pages past the end is an error naming the page count and the valid pages, not an empty "showing all" text.
	// Arrange
	if exec.Command("python3", "-c", "import pypdf").Run() != nil {
		t.Skip("python3 with pypdf is required")
	}
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext is required")
	}
	f, _ := newHostFixture(t, nil)
	ctx := tools.WithCall(context.Background(), "w", &recorder{})
	if _, err := f.call(ctx, "execute_command", `{"command":"python3 -c 'from pypdf import PdfWriter; w = PdfWriter(); [w.add_blank_page(200, 100) for _ in range(3)]; w.write(\"report.pdf\")'"}`); err != nil {
		t.Fatal(err)
	}

	// Act
	_, err := f.call(ctx, "read_file", `{"path":"report.pdf","pages":"7-9"}`)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "past the end") || !strings.Contains(err.Error(), "3 pages") || !strings.Contains(err.Error(), "valid pages: 1-3") {
		t.Fatalf("err %v", err)
	}
}
