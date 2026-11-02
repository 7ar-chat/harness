package skillsfs_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// opsPython returns a python3 that has openpyxl, python-docx and lxml (HARNESS_TEST_PYTHON, else python3), or skips.
func opsPython(t *testing.T) string {
	t.Helper()
	py := os.Getenv("HARNESS_TEST_PYTHON")
	if py == "" {
		py = "python3"
	}
	if err := exec.Command(py, "-c", "import openpyxl, docx, lxml").Run(); err != nil {
		t.Skipf("%s lacks openpyxl/python-docx/lxml (set HARNESS_TEST_PYTHON): %v", py, err)
	}
	return py
}

// runOps runs a skill script with args (stdin optional) and decodes its JSON stdout; code is the exit status.
func runOps(t *testing.T, py, script, stdin string, args ...string) (out map[string]any, code int) {
	t.Helper()
	cmd := exec.Command(py, append([]string{filepath.Join("builtin", script)}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	raw, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s %v: not JSON (%v): %s", script, args, err, raw)
	}
	return out, code
}

// failureOf returns the first record of a failure report.
func failureOf(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	list, _ := out["failures"].([]any)
	if len(list) == 0 {
		t.Fatalf("no failures in %v", out)
	}
	return list[0].(map[string]any)
}

func TestBuildAndEditXlsxByOps(t *testing.T) {
	// build_xlsx.py builds a workbook from the example spec; edit_xlsx.py rejects a bad batch whole (the file is untouched, the failure names the sheet it meant), then applies a good one; inspect_xlsx.py reads a bounded range.
	// Arrange
	py := opsPython(t)
	dir := t.TempDir()
	book := filepath.Join(dir, "book.xlsx")
	example, err := exec.Command(py, "builtin/xlsx/scripts/build_xlsx.py", "--example").Output()
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	_ = json.Unmarshal(example, &spec)
	delete(spec["sheets"].([]any)[1].(map[string]any), "charts") // a workbook with charts is refused by edit_xlsx.py (openpyxl would drop them)
	specJSON, _ := json.Marshal(spec)

	// Act
	built, _ := runOps(t, py, "xlsx/scripts/build_xlsx.py", string(specJSON), "--spec", "-", "--out", book)
	before, _ := os.ReadFile(book)
	bad, badCode := runOps(t, py, "xlsx/scripts/edit_xlsx.py", `[{"op":"set_cell","sheet":"Plann","cell":"B2","value":1},{"op":"formt_range","range":"A1"}]`, book, "--ops", "-", "--inplace")
	after, _ := os.ReadFile(book)
	good, goodCode := runOps(t, py, "xlsx/scripts/edit_xlsx.py", `[{"op":"set_cell","sheet":"Plan","cell":"B2","value":2000},{"op":"append_rows","sheet":"Plan","rows":[["Dec",1,2,"=B6-C6"]]}]`, book, "--ops", "-", "--inplace")
	rng, _ := runOps(t, py, "xlsx/scripts/inspect_xlsx.py", "", book, "--sheet", "Plan", "--range", "A1:E6", "--max-rows", "2")

	// Assert
	if built["ok"] != true || built["next_step"] == nil {
		t.Errorf("build: %v", built)
	}
	if badCode != 1 || bad["reason"] != "sheet_not_found" {
		t.Errorf("bad batch: code %d, %v", badCode, bad)
	}
	if f := failureOf(t, bad); f["did_you_mean"] != "Plan" || f["index"] != float64(0) {
		t.Errorf("failure record lacks the fix: %v", f)
	}
	if string(before) != string(after) {
		t.Error("a rejected batch changed the file")
	}
	if goodCode != 0 || good["applied"] != float64(2) || good["formulas_touched"] != true {
		t.Errorf("good batch: code %d, %v", goodCode, good)
	}
	if note, _ := rng["note"].(string); !strings.Contains(note, "continue with --range A3:E") || rng["rowsTotal"] != float64(6) {
		t.Errorf("a cut range must say how to continue: %v", rng)
	}
}

func TestBuildAndEditDocxFromMarkdownAndOps(t *testing.T) {
	// new_document.py --md builds a document from Markdown; edit_docx.py addresses paragraphs by the original index (indexes do not shift inside a batch), rejects an out-of-range index with the valid range, and inspect_docx.py --text --range reads a slice.
	// Arrange
	py := opsPython(t)
	dir := t.TempDir()
	md, doc := filepath.Join(dir, "r.md"), filepath.Join(dir, "r.docx")
	_ = os.WriteFile(md, []byte("---\ntitle: T\nlocale: en\n---\n# Summary\nRevenue **grew**.\n\n- one\n- two\n\n# Next\nDone.\n"), 0o644)

	// Act
	built, _ := runOps(t, py, "docx/scripts/new_document.py", "", "--md", md, "--out", doc)
	bad, badCode := runOps(t, py, "docx/scripts/edit_docx.py", `[{"op":"set_text","index":99,"text":"x"}]`, doc, "--ops", "-", "--inplace")
	good, goodCode := runOps(t, py, "docx/scripts/edit_docx.py", `[{"op":"insert_after","index":3,"blocks":[{"heading":"Risks"}]},{"op":"set_text","index":3,"text":"Revenue **fell**."}]`, doc, "--ops", "-", "--inplace")
	text, err := exec.Command(py, "builtin/docx/scripts/inspect_docx.py", doc, "--text", "--range", "3-5").Output()
	if err != nil {
		t.Fatal(err)
	}

	// Assert
	if built["paragraphs"] == nil {
		t.Errorf("build: %v", built)
	}
	if badCode != 1 || failureOf(t, bad)["reason"] != "out_of_range" || failureOf(t, bad)["valid_range"] == nil {
		t.Errorf("bad index: code %d, %v", badCode, bad)
	}
	if goodCode != 0 || good["applied"] != float64(2) {
		t.Errorf("good batch: code %d, %v", goodCode, good)
	}
	lines := strings.Split(strings.TrimSpace(string(text)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "Revenue fell.") || !strings.Contains(lines[1], "(Heading 1) Risks") {
		t.Errorf("document after the batch: %q", lines)
	}
}
