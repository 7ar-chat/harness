package sandboxtools_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/coding/sandboxtools"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

func TestViewFileValidatesBeforeRendering(t *testing.T) {
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.Put("/workspace/notes.txt", "x")
		sb.PutDoc("/workspace/d.pdf", sandboxtest.Doc{Parts: []string{"p"}})
	})
	vision := withRec(&recorder{vision: true})

	// Act
	_, blank := f.call(vision, "view_file", `{"path":""}`)
	_, blind := f.call(withRec(&recorder{}), "view_file", `{"path":"d.pdf"}`)
	_, text := f.call(vision, "view_file", `{"path":"notes.txt"}`)
	_, missing := f.call(vision, "view_file", `{"path":"gone.pdf"}`)

	// Assert
	if blank == nil || !strings.Contains(blank.Error(), "path is required") {
		t.Errorf("blank: %v", blank)
	}
	if blind == nil || !strings.Contains(blind.Error(), sandboxtools.NoVisionError) {
		t.Errorf("blind: %v", blind)
	}
	if text == nil || !strings.Contains(text.Error(), "read /workspace/notes.txt with read_file") {
		t.Errorf("text file: %v", text)
	}
	if missing == nil || !strings.Contains(missing.Error(), "/workspace/gone.pdf: no such file or directory") {
		t.Errorf("missing: %v", missing)
	}
	if len(f.sb.Renders()) != 0 {
		t.Errorf("renders written: %v", f.sb.Renders())
	}
}

func TestViewFileShowsAnImageAsIsWithoutRendering(t *testing.T) {
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.Put("/workspace/logo.webp", "RIFFdata")
		sb.Put("/workspace/huge.gif", strings.Repeat("x", 5<<20+1))
	})
	rec := &recorder{vision: true}

	// Act
	out, err := f.call(withRec(rec), "view_file", `{"path":"logo.webp"}`)
	_, big := f.call(withRec(rec), "view_file", `{"path":"huge.gif"}`)
	_, missing := f.call(withRec(rec), "view_file", `{"path":"gone.png"}`)

	// Assert
	var r viewResult
	_ = json.Unmarshal([]byte(out), &r)
	if err != nil || r.TotalPages != 1 || len(r.Pages) != 1 || r.Pages[0].ImagePath != "/workspace/logo.webp" {
		t.Errorf("image: %s %v", out, err)
	}
	if len(rec.images) != 1 || rec.images[0].MIMEType != "image/webp" {
		t.Errorf("images %+v", rec.images)
	}
	if big == nil || !strings.Contains(big.Error(), "cannot be shown") || missing == nil || !strings.Contains(missing.Error(), "no such file") {
		t.Errorf("big %v missing %v", big, missing)
	}
	if len(f.sb.Renders()) != 0 {
		t.Errorf("renders written: %v", f.sb.Renders())
	}
}

// renderingScript answers the render script with a report for pages and writes the PNGs of the pages in written.
func renderingScript(sb **scripted, report string, written ...int) func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
	return func(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		for _, n := range written {
			_ = (*sb).WriteFile(ctx, fmt.Sprintf("%s/p%d.png", req.Env["HARNESS_OUT"], n), []byte("png"))
		}
		return printing(strings.ReplaceAll(report, "OUT", req.Env["HARNESS_OUT"])+"\n", 0)(ctx, req)
	}
}

func TestViewFileNamesThePagesThatDoNotExist(t *testing.T) {
	cases := map[string]struct{ pastEnd, want string }{
		"one page": {"7", "page 7 does not exist (deck.pptx has 3 slides)"},
		"a range":  {"4-5", "pages 4-5 do not exist (deck.pptx has 3 slides)"},
		"a list":   {"6,9", "pages 6,9 do not exist (deck.pptx has 3 slides)"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			var sb *scripted
			sb = newScripted(map[string]func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error){
				"# work:render": renderingScript(&sb, `{"totalPages":3,"size":"3 slides","pastEnd":"`+c.pastEnd+`","pages":[{"page":1,"imagePath":"OUT/p1.png"}]}`, 1)})
			sb.Put("/workspace/deck.pptx", "pptx")
			ts := toolsOn(t, sb, sandboxtools.Options{})

			// Act
			out, err := ts["view_file"].InvokableRun(withRec(&recorder{vision: true}), `{"path":"deck.pptx","pages":"1,`+c.pastEnd+`"}`)

			// Assert
			var r viewResult
			_ = json.Unmarshal([]byte(out), &r)
			if err != nil || r.Note != c.want || len(r.Pages) != 1 {
				t.Fatalf("err %v out %s", err, out)
			}
		})
	}
}

func TestViewFileSkipsPagesWhoseImageCannotBeReadAndFailsWhenNoneCan(t *testing.T) {
	// Arrange: the renderer reports 3 pages but only page 2 has an image on disk.
	var sb *scripted
	sb = newScripted(map[string]func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error){
		"# work:render": renderingScript(&sb, `{"totalPages":3,"pages":[{"page":1,"imagePath":"OUT/p1.png"},{"page":2,"imagePath":"OUT/p2.png"},{"page":3,"imagePath":"OUT/p3.png"}]}`, 2)})
	sb.Put("/workspace/a.pdf", "pdf")
	sb.Put("/workspace/b.pdf", "pdf")
	ts := toolsOn(t, sb, sandboxtools.Options{})
	rec := &recorder{vision: true}

	// Act
	out, err := ts["view_file"].InvokableRun(withRec(rec), `{"path":"a.pdf"}`)
	sb.scripts["# work:render"] = renderingScript(&sb, `{"totalPages":2,"pages":[{"page":1,"imagePath":"OUT/p1.png"}]}`)
	_, none := ts["view_file"].InvokableRun(withRec(rec), `{"path":"b.pdf","pages":"1"}`)

	// Assert
	var r viewResult
	_ = json.Unmarshal([]byte(out), &r)
	if err != nil || len(r.Pages) != 1 || r.Pages[0].Page != 2 || len(rec.images) != 1 {
		t.Fatalf("err %v out %s images %d", err, out, len(rec.images))
	}
	if none == nil || !strings.Contains(none.Error(), `no page of /workspace/b.pdf could be rendered (pages "1" of 2)`) || len(rec.images) != 1 {
		t.Fatalf("none: %v (images %d)", none, len(rec.images))
	}
}

func TestViewFileReportsTheRendererErrorAndScriptFailures(t *testing.T) {
	cases := map[string]struct {
		script func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error)
		want   string
	}{
		"renderer error": {printing(`{"error":"LibreOffice could not render a.pdf (exit 81)"}`+"\n", 0), "LibreOffice could not render a.pdf (exit 81)"},
		"no result":      {printing("segfault\n", 139), "sandbox script gave no result (exit 139)"},
		"exec error": {func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
			return sandbox.ExecResult{}, errors.New("boom")
		}, "sandbox script failed: boom"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			sb := newScripted(map[string]func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error){"# work:render": c.script})
			sb.Put("/workspace/a.pdf", "pdf")
			ts := toolsOn(t, sb, sandboxtools.Options{})
			rec := &recorder{vision: true}

			// Act
			_, err := ts["view_file"].InvokableRun(withRec(rec), `{"path":"a.pdf"}`)

			// Assert
			if err == nil || !strings.Contains(err.Error(), c.want) || len(rec.images) != 0 {
				t.Fatalf("err %v images %d", err, len(rec.images))
			}
		})
	}
}

func TestViewFileSaysWhenThereAreMorePagesThanItShows(t *testing.T) {
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.PutDoc("/workspace/long.pdf", sandboxtest.Doc{Parts: []string{"1", "2", "3", "4", "5", "6", "7", "8"}})
	})

	// Act
	all, err1 := f.call(withRec(&recorder{vision: true}), "view_file", `{"path":"long.pdf"}`)
	some, err2 := f.call(withRec(&recorder{vision: true}), "view_file", `{"path":"long.pdf","pages":"7-8"}`)

	// Assert
	var a, s viewResult
	_ = json.Unmarshal([]byte(all), &a)
	_ = json.Unmarshal([]byte(some), &s)
	if err1 != nil || len(a.Pages) != 6 || a.TotalPages != 8 || a.Note != "showing pages 1-6 of 8; ask for others with pages (at most 6 per call)" {
		t.Errorf("all: %v %s", err1, all)
	}
	if err2 != nil || len(s.Pages) != 2 || s.Note != "" {
		t.Errorf("explicit pages need no hint: %v %s", err2, some)
	}
}

func TestViewFileRendersEachFileVersionToItsOwnFolder(t *testing.T) {
	// Arrange: the same name in two directories, and an unsafe name.
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.PutDoc("/workspace/a/report.pdf", sandboxtest.Doc{Parts: []string{"x"}})
		sb.PutDoc("/workspace/b/report.pdf", sandboxtest.Doc{Parts: []string{"x"}})
		sb.PutDoc("/workspace/my report (final) é.pdf", sandboxtest.Doc{Parts: []string{"x"}})
		sb.PutDoc("/workspace/"+strings.Repeat("n", 90)+".pdf", sandboxtest.Doc{Parts: []string{"x"}})
	})
	id := func(path string) string {
		out, err := f.call(withRec(&recorder{vision: true}), "view_file", `{"path":"`+path+`"}`)
		if err != nil {
			t.Fatal(err)
		}
		m := decodeMap(t, out)
		return m["renderId"].(string)
	}

	// Act
	a, b, odd, long, again := id("a/report.pdf"), id("b/report.pdf"), id("my report (final) é.pdf"), id(strings.Repeat("n", 90)+".pdf"), id("a/report.pdf")

	// Assert
	if a == b || a != again || !strings.HasPrefix(a, "report-") || !strings.HasPrefix(odd, "my_report_final_") ||
		!strings.HasPrefix(long, strings.Repeat("n", 60)+"-") || len(long) != 60+1+10 {
		t.Fatalf("ids: %q %q %q %q", a, b, odd, long)
	}
}

func TestViewFileSurvivesAFailureToCreateItsRenderFolder(t *testing.T) {
	// Arrange
	sb := newScripted(nil)
	sb.Put("/workspace/a.pdf", "pdf")
	ts := toolsOn(t, &noMkdir{scripted: sb}, sandboxtools.Options{})

	// Act
	_, err := ts["view_file"].InvokableRun(withRec(&recorder{vision: true}), `{"path":"a.pdf"}`)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "read-only file system") {
		t.Fatalf("err %v", err)
	}
}

// noMkdir is a sandbox whose MkdirAll fails.
type noMkdir struct{ *scripted }

func (noMkdir) MkdirAll(context.Context, string) error { return errors.New("read-only file system") }
