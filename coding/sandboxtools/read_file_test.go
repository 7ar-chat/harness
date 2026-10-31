package sandboxtools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/engine/tools"
)

func TestReadFileReturnsNumberedLines(t *testing.T) {
	// read_file resolves relative paths under /workspace and numbers lines like cat -n.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/notes.txt", "alpha\nbeta\n") })

	// Act
	out, err := f.call(context.Background(), "read_file", `{"path":"notes.txt"}`)

	// Assert
	if err != nil || out != "     1\talpha\n     2\tbeta\n" {
		t.Fatalf("err %v out %q", err, out)
	}
}

func TestReadFilePagesWithOffsetAndLimit(t *testing.T) {
	// offset/limit select lines and the output tells how to continue.
	// Arrange
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/l.txt", b.String()) })

	// Act
	out, err := f.call(context.Background(), "read_file", `{"path":"/workspace/l.txt","offset":3,"limit":2}`)

	// Assert
	if err != nil || !strings.HasPrefix(out, "     3\tline 3\n     4\tline 4\n") || !strings.Contains(out, "Use offset=5") {
		t.Fatalf("err %v out %q", err, out)
	}
}

var nextOffset = regexp.MustCompile(`Showing lines (\d+)-(\d+) of (\d+)\. Use offset=(\d+) to read more\.`)

func TestReadFilePagesStayBelowTheOutputLimitAndNeverSkipLines(t *testing.T) {
	// A long file is read in pages of at most ~18,000 characters; following each footer's offset visits every line exactly once (A-04).
	// Arrange
	var b strings.Builder
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&b, "row %04d,%s\n", i, strings.Repeat("v", 40))
	}
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/export.csv", b.String()) })

	// Act
	seen := map[int]int{}
	offset, pages := 1, 0
	for offset > 0 && pages < 100 {
		out, err := f.call(context.Background(), "read_file", fmt.Sprintf(`{"path":"export.csv","offset":%d}`, offset))
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if len(out) > 18500 {
			t.Fatalf("page of %d chars", len(out))
		}
		for _, line := range strings.Split(out, "\n") {
			if n, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(line, "\t", 2)[0])); err == nil && strings.Contains(line, "\t") {
				seen[n]++
			}
		}
		offset = 0
		if m := nextOffset.FindStringSubmatch(out); m != nil {
			offset, _ = strconv.Atoi(m[4])
			if end, _ := strconv.Atoi(m[2]); end+1 != offset {
				t.Fatalf("footer %q", m[0])
			}
		}
	}

	// Assert
	if len(seen) != 5000 || pages < 10 {
		t.Fatalf("lines seen %d in %d pages", len(seen), pages)
	}
	for n, c := range seen {
		if c != 1 {
			t.Fatalf("line %d seen %d times", n, c)
		}
	}
}

func TestReadFileReportsMissingAndBinaryFiles(t *testing.T) {
	// Missing files and binary files that are neither documents nor images are clear tool errors.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/blob.bin", "\x7fELF\x00\x01") })

	// Act
	_, missing := f.call(context.Background(), "read_file", `{"path":"nope.txt"}`)
	_, binary := f.call(context.Background(), "read_file", `{"path":"blob.bin"}`)

	// Assert
	if missing == nil || !strings.Contains(missing.Error(), "no such file") || binary == nil || !strings.Contains(binary.Error(), "binary") {
		t.Fatalf("missing %v binary %v", missing, binary)
	}
}

func TestReadFileTellsWhenFileExceedsByteCap(t *testing.T) {
	// Files over 5 MiB are read partially with a note to the model.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.Put("/workspace/huge.log", strings.Repeat("0123456789\n", 600000))
	})

	// Act
	out, err := f.call(context.Background(), "read_file", `{"path":"huge.log","limit":1}`)

	// Assert
	if err != nil || !strings.Contains(out, "only the first 5242880 bytes were read") {
		t.Fatalf("err %v out %q", err, out[:min(len(out), 300)])
	}
}

func TestReadFileExtractsSelectedPdfPages(t *testing.T) {
	// A PDF is read as extracted text of the requested pages, after a header giving the page count and the pages shown.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.PutDoc("/workspace/contract.pdf", sandboxtest.Doc{Parts: []string{"Preamble", "Article 1 - Scope", "Article 2 - Price"}})
	})

	// Act
	out, err := f.call(context.Background(), "read_file", `{"path":"contract.pdf","pages":"2-3"}`)

	// Assert
	if err != nil || !strings.HasPrefix(out, "[document: 3 pages; showing pages 2,3]\n") || !strings.Contains(out, "Article 1 - Scope") ||
		!strings.Contains(out, "Article 2 - Price") || strings.Contains(out, "Preamble") {
		t.Fatalf("err %v out %q", err, out)
	}
}

func TestReadFileExtractsOneWorkbookSheet(t *testing.T) {
	// sheet selects one XLSX sheet; an unknown sheet is an error listing the sheets.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) {
		sb.PutDoc("/workspace/book.xlsx", sandboxtest.Doc{Names: []string{"Data", "Summary"}, Parts: []string{"| a | 1 |", "| total | 9 |"}})
	})

	// Act
	out, err := f.call(context.Background(), "read_file", `{"path":"book.xlsx","sheet":"Summary"}`)
	_, unknown := f.call(context.Background(), "read_file", `{"path":"book.xlsx","sheet":"Nope"}`)

	// Assert
	if err != nil || !strings.HasPrefix(out, "[document: 2 sheets; showing sheet Summary]") || !strings.Contains(out, "| total | 9 |") || strings.Contains(out, "| a | 1 |") {
		t.Fatalf("err %v out %q", err, out)
	}
	if unknown == nil || !strings.Contains(unknown.Error(), "Data, Summary") {
		t.Fatalf("unknown %v", unknown)
	}
}

func TestReadFileShowsImagesToVisionModels(t *testing.T) {
	// An image is returned as {path, image: true} and attached for a model that can see images; a model that cannot gets a note instead.
	// Arrange
	f := newFixture(t, func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/chart.png", "\x89PNG\r\n\x1a\n\x00data") })
	vision, blind := &recorder{vision: true}, &recorder{}

	// Act
	out, err := f.call(tools.WithCall(context.Background(), "c1", vision), "read_file", `{"path":"chart.png"}`)
	blindOut, _ := f.call(tools.WithCall(context.Background(), "c2", blind), "read_file", `{"path":"chart.png"}`)

	// Assert
	var res map[string]any
	_ = json.Unmarshal([]byte(out), &res)
	if err != nil || res["image"] != true || res["path"] != "/workspace/chart.png" || len(vision.images) != 1 ||
		vision.images[0].MIMEType != "image/png" || string(vision.images[0].Data) != "\x89PNG\r\n\x1a\n\x00data" {
		t.Fatalf("err %v out %s images %d", err, out, len(vision.images))
	}
	if len(blind.images) != 0 || !strings.Contains(blindOut, "cannot view images") {
		t.Fatalf("blind %s", blindOut)
	}
}
