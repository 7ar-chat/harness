package sandboxtools_test

import (
	"context"
	"encoding/json"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/tools"
	"github.com/GhaziBenDahmane/harness/tools/sandboxtools"
)

type viewResult struct {
	Path  string `json:"path"`
	Pages []struct {
		Page      int    `json:"page"`
		ImagePath string `json:"imagePath"`
	} `json:"pages"`
	TotalPages int    `json:"totalPages"`
	Note       string `json:"note"`
}

func TestViewFileRendersPagesAndShowsThemToTheModel(t *testing.T) {
	// view_file renders the requested pages to PNGs under /workspace/.work/renders/ and attaches each one as an image.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.PutDoc("/workspace/outputs/deck.pptx", sandboxtest.Doc{Parts: []string{"Title", "Agenda", "Results"}})
	})
	rec := &recorder{vision: true}

	// Act
	out, err := f.call(tools.WithCall(context.Background(), "v1", rec), "view_file", `{"path":"outputs/deck.pptx","pages":"2-3"}`)

	// Assert
	var r viewResult
	_ = json.Unmarshal([]byte(out), &r)
	if err != nil || r.Path != "/workspace/outputs/deck.pptx" || r.TotalPages != 3 || len(r.Pages) != 2 || r.Pages[0].Page != 2 ||
		!strings.HasPrefix(r.Pages[0].ImagePath, "/workspace/.work/renders/deck-") {
		t.Fatalf("err %v out %s", err, out)
	}
	if len(rec.images) != 2 || rec.images[1].Path != r.Pages[1].ImagePath || rec.images[1].MIMEType != "image/png" {
		t.Fatalf("images %+v", rec.images)
	}
}

func TestViewFileCreatesItsRenderDirectoryBeforeRendering(t *testing.T) {
	// view_file creates the file's render directory under /workspace/.work/renders/ itself, before the render runs.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.PutDoc("/workspace/outputs/memo.docx", sandboxtest.Doc{Parts: []string{"Page"}})
	})
	rec := &recorder{vision: true}

	// Act
	out, err := f.call(tools.WithCall(context.Background(), "v1", rec), "view_file", `{"path":"outputs/memo.docx"}`)

	// Assert
	var r viewResult
	_ = json.Unmarshal([]byte(out), &r)
	if err != nil || len(r.Pages) != 1 {
		t.Fatalf("err %v out %s", err, out)
	}
	dir := path.Dir(r.Pages[0].ImagePath)
	if !strings.HasPrefix(dir, "/workspace/.work/renders/memo-") || !slices.Contains(f.sb.Mkdirs(), dir) {
		t.Fatalf("render dir %s not created by view_file (mkdirs %v)", dir, f.sb.Mkdirs())
	}
}

func TestViewFileShowsAtMostSixPages(t *testing.T) {
	// Without pages, the first six pages are rendered and a note says how to see the others.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.PutDoc("/workspace/outputs/report.pdf", sandboxtest.Doc{Parts: []string{"1", "2", "3", "4", "5", "6", "7", "8"}})
	})
	rec := &recorder{vision: true}

	// Act
	out, err := f.call(tools.WithCall(context.Background(), "v1", rec), "view_file", `{"path":"outputs/report.pdf"}`)

	// Assert
	var r viewResult
	_ = json.Unmarshal([]byte(out), &r)
	if err != nil || len(r.Pages) != 6 || r.TotalPages != 8 || !strings.Contains(r.Note, "1-6 of 8") || len(rec.images) != 6 {
		t.Fatalf("err %v out %s", err, out)
	}
}

func TestViewFileRefusesModelsWithoutVision(t *testing.T) {
	// A model that cannot see images gets an error pointing to read_file and the inspect script; nothing is rendered.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.PutDoc("/workspace/outputs/report.pdf", sandboxtest.Doc{Parts: []string{"1"}})
	})

	// Act
	_, err := f.call(tools.WithCall(context.Background(), "v1", &recorder{}), "view_file", `{"path":"outputs/report.pdf"}`)

	// Assert
	if err == nil || !strings.Contains(err.Error(), sandboxtools.NoVisionError) || len(f.backend.Created()) != 0 {
		t.Fatalf("err %v sandboxes %d", err, len(f.backend.Created()))
	}
}

type searchResult struct {
	Matches []struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Text string `json:"text"`
	} `json:"matches"`
	Truncated bool `json:"truncated"`
}

func TestSearchFilesReturnsMatchingLines(t *testing.T) {
	// search_files finds regex matches with path and line number, filtered by glob, case-insensitively when asked, including saved outputs under .work.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.Put("/workspace/a.py", "import os\nTOTAL = 1\n")
		sb.Put("/workspace/notes.md", "total is 3\n")
		sb.Put("/workspace/.work/tool-output/c1.txt", "line\nTotal: 42\n")
	})

	// Act
	out, err := f.call(context.Background(), "search_files", `{"pattern":"total","ignoreCase":true}`)
	py, _ := f.call(context.Background(), "search_files", `{"pattern":"total","ignoreCase":true,"glob":"*.py"}`)

	// Assert
	var r, p searchResult
	_ = json.Unmarshal([]byte(out), &r)
	_ = json.Unmarshal([]byte(py), &p)
	if err != nil || len(r.Matches) != 3 || r.Truncated {
		t.Fatalf("err %v out %s", err, out)
	}
	if len(p.Matches) != 1 || p.Matches[0].Path != "/workspace/a.py" || p.Matches[0].Line != 2 || p.Matches[0].Text != "TOTAL = 1" {
		t.Fatalf("py %s", py)
	}
}

func TestSearchFilesCapsResults(t *testing.T) {
	// More matches than maxResults are cut and flagged truncated.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/log.txt", strings.Repeat("error x\n", 50)) })

	// Act
	out, err := f.call(context.Background(), "search_files", `{"pattern":"error","maxResults":10}`)

	// Assert
	var r searchResult
	_ = json.Unmarshal([]byte(out), &r)
	if err != nil || len(r.Matches) != 10 || !r.Truncated {
		t.Fatalf("err %v out %s", err, out)
	}
}

func TestSearchFilesReportsAnInvalidPattern(t *testing.T) {
	// An invalid regular expression is a tool error with the parser's message.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/a.txt", "x") })

	// Act
	_, err := f.call(context.Background(), "search_files", `{"pattern":"("}`)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "regex parse error") {
		t.Fatalf("err %v", err)
	}
}

func TestSearchFilesFallbackWorksWithoutRipgrep(t *testing.T) {
	// In a real shell without ripgrep (Python fallback), search_files finds the same matches with line numbers and skips .git.
	// Arrange
	requirePython3(t)
	f, hb := newHostFixture(t, map[string]string{"WORK_SEARCH_ENGINE": "python"})
	if _, err := f.call(context.Background(), "write_file", `{"path":"data/q3.csv","content":"region,amount\nNorth,120\nSouth,80\n"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.call(context.Background(), "write_file", `{"path":".git/config","content":"North,1\n"}`); err != nil {
		t.Fatal(err)
	}

	// Act
	out, err := f.call(context.Background(), "search_files", `{"pattern":"^(North|South),"}`)

	// Assert
	var r searchResult
	_ = json.Unmarshal([]byte(out), &r)
	if err != nil || len(r.Matches) != 2 || r.Matches[0].Path != "/workspace/data/q3.csv" || r.Matches[0].Line != 2 || r.Matches[1].Text != "South,80" {
		t.Fatalf("err %v out %s", err, out)
	}
	if hb.Created()[0].Commands() == 0 {
		t.Fatal("no command ran")
	}
}
