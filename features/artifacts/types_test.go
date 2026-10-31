package artifacts_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/7ar-chat/harness/features/artifacts"
)

func testdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

const (
	mimeDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	mimeXLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	mimePPTX = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
)

func TestMimeTypeOfAFileName(t *testing.T) {
	// The content type comes from the (case-insensitive) extension: Office/text types win over the system table, unknown ones are octet-stream, parameters are dropped.
	cases := []struct{ name, want string }{
		{"report.DOCX", mimeDOCX},
		{"data.csv", "text/csv"},
		{"/workspace/outputs/notes.md", "text/markdown"},
		{"style.css", "text/css"},
		{"archive.unknownext", "application/octet-stream"},
		{"Makefile", "application/octet-stream"},
	}
	for _, c := range cases {
		// Act
		got := artifacts.MimeType(c.name)

		// Assert
		if got != c.want {
			t.Errorf("MimeType(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPreviewableKindOfAFileName(t *testing.T) {
	// Office documents and PDFs preview as pdf, images as image, HTML as html, source/text as text, anything else not at all.
	cases := []struct{ name, want string }{
		{"a.pdf", "pdf"}, {"a.XLSX", "pdf"}, {"a.odp", "pdf"},
		{"a.PNG", "image"}, {"a.svg", "image"},
		{"a.html", "html"}, {"a.htm", "html"},
		{"a.go", "text"}, {"a.CSV", "text"}, {"a.md", "text"},
		{"a.zip", "none"}, {"noext", "none"}, {"a.mp4", "none"},
	}
	for _, c := range cases {
		// Act
		got := artifacts.Previewable(c.name)

		// Assert
		if got != c.want {
			t.Errorf("Previewable(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestContentMimeTrustsASignatureTheExtensionDoesNotClaim(t *testing.T) {
	// A PDF or Office file saved under a text/unknown extension is recorded with the signature's type; honest files keep their extension's type.
	pdf := testdata(t, "memo.pdf")
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"memo.txt", pdf, "application/pdf"},
		{"report.bin", testdata(t, "report.docx"), mimeDOCX},
		{"sales.dat", testdata(t, "sales.xlsx"), mimeXLSX},
		{"deck", testdata(t, "deck.pptx"), mimePPTX},
		{"notes.txt", []byte("just text"), "text/plain"},
		{"memo.pdf", pdf, "application/pdf"},
		{"photo.png", []byte("not really a png"), "image/png"},
	}
	for _, c := range cases {
		// Act
		got := artifacts.ContentMime(c.name, c.data)

		// Assert
		if got != c.want {
			t.Errorf("ContentMime(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPreviewableAsRefusesAFileWhoseRecordedTypeIsNotItsExtensions(t *testing.T) {
	// A PDF renamed .txt (recorded application/pdf) is never previewed as text; types that merely differ keep the extension's preview.
	cases := []struct{ name, mime, want string }{
		{"notes.txt", "text/plain", "text"},
		{"notes.txt", "", "text"},
		{"notes.txt", "application/pdf", "none"},
		{"notes.txt", "application/octet-stream", "none"},
		{"notes.txt", mimeDOCX, "none"},
		{"notes.txt", "text/x-custom", "text"},
		{"memo.pdf", "application/pdf", "pdf"},
	}
	for _, c := range cases {
		// Act
		got := artifacts.PreviewableAs(c.name, c.mime)

		// Assert
		if got != c.want {
			t.Errorf("PreviewableAs(%q, %q) = %q, want %q", c.name, c.mime, got, c.want)
		}
	}
}

func TestSanitizeFilenameKeepsASafeSingleComponent(t *testing.T) {
	// Paths, backslashes, control characters, quotes, bad UTF-8 and over-long names are normalised to one safe file name.
	cases := []struct{ in, want string }{
		{"report.docx", "report.docx"},
		{"a/b/c.txt", "c.txt"},
		{`..\..\evil.txt`, "evil.txt"},
		{"../../etc/passwd", "passwd"},
		{"", "upload"},
		{"  . ", "upload"},
		{"dir/", "dir"},
		{"a\x00b\x1fc.txt", "a_b_c.txt"},
		{`say "hi".txt`, `say 'hi'.txt`},
		{"a\xffb.txt", "a_b.txt"},
		{" spaced name ", "spaced name"},
	}
	for _, c := range cases {
		// Act
		got := artifacts.SanitizeFilename(c.in)

		// Assert
		if got != c.want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeFilenameShortensLongNamesKeepingTheExtension(t *testing.T) {
	// Names above 200 bytes are cut on a rune boundary, keeping a reasonable extension and dropping an absurd one.
	t.Run("keeps a short extension", func(t *testing.T) {
		// Act
		got := artifacts.SanitizeFilename(strings.Repeat("a", 300) + ".txt")

		// Assert
		if len(got) != 200 || !strings.HasSuffix(got, ".txt") {
			t.Fatalf("got %d bytes %q", len(got), got[len(got)-10:])
		}
	})
	t.Run("drops an extension above 20 bytes", func(t *testing.T) {
		// Act
		got := artifacts.SanitizeFilename(strings.Repeat("a", 250) + "." + strings.Repeat("b", 30))

		// Assert
		if len(got) != 200 || strings.Contains(got, ".") {
			t.Fatalf("got %d bytes", len(got))
		}
	})
	t.Run("cuts multi-byte names on a rune boundary", func(t *testing.T) {
		// Act
		got := artifacts.SanitizeFilename(strings.Repeat("é", 150) + ".md")

		// Assert
		if len(got) > 200 || !utf8.ValidString(got) || !strings.HasSuffix(got, ".md") {
			t.Fatalf("got %d bytes valid=%v", len(got), utf8.ValidString(got))
		}
	})
}

func TestUploadTargetAcceptsOnlyFilePathsUnderTheWorkspace(t *testing.T) {
	// A target path is relative to or under /workspace, with no traversal, control characters, directories skipped by snapshots, or names that need sanitising.
	valid := []struct{ in, dir, name string }{
		{"report.csv", "/workspace", "report.csv"},
		{"outputs/report.csv", "/workspace/outputs", "report.csv"},
		{"/workspace/outputs/a/b.txt", "/workspace/outputs/a", "b.txt"},
		{`outputs\win.txt`, "/workspace/outputs", "win.txt"},
		{"  spaced/x.txt  ", "/workspace/spaced", "x.txt"},
		{"a//b.txt", "/workspace/a", "b.txt"},
	}
	for _, c := range valid {
		// Act
		dir, name, err := artifacts.UploadTarget(c.in)

		// Assert
		if err != nil || dir != c.dir || name != c.name {
			t.Errorf("UploadTarget(%q) = %q %q %v, want %q %q", c.in, dir, name, err, c.dir, c.name)
		}
	}
	invalid := []string{
		"", "   ", "/etc/passwd", "/workspace", "/workspace/", "outputs/", "../x.txt", "a/../../x.txt", "/workspace/a/../b.txt",
		".git/config", "src/.git/hooks/x", "node_modules/pkg/index.js", "app/__pycache__/m.pyc", ".venv/bin/python", ".work/scratch.txt",
		"a\x00b.txt", "dir/line\nbreak.txt", "bad\xff.txt", `dir/say "hi".txt`, "dir/ lead.txt ",
	}
	for _, in := range invalid {
		// Act
		_, _, err := artifacts.UploadTarget(in)

		// Assert
		if !errors.Is(err, artifacts.ErrInvalidTargetPath) {
			t.Errorf("UploadTarget(%q) err = %v, want ErrInvalidTargetPath", in, err)
		}
	}
}

func TestInSkippedDirMatchesTheDirectoriesSnapshotsSkip(t *testing.T) {
	// Files under .work or a node_modules/.git/__pycache__/.venv directory (at any depth) are never artifacts; a file merely named like one is.
	cases := map[string]bool{
		"/workspace/.work/x.txt":              true,
		"/workspace/.work":                    true,
		"/workspace/a/node_modules/b/c.js":    true,
		"/workspace/.git/HEAD":                true,
		"/workspace/src/__pycache__/m.pyc":    true,
		"/workspace/.venv/lib/x.py":           true,
		"/workspace/outputs/report.docx":      false,
		"/workspace/node_modules":             false,
		"/workspace/notes/.gitignore":         false,
		"/workspace/./a/../.work/../y/z.txt":  false,
		"/workspace/a/../node_modules/q/z.js": true,
	}
	for p, want := range cases {
		// Act
		got := artifacts.InSkippedDir(p)

		// Assert
		if got != want {
			t.Errorf("InSkippedDir(%q) = %v, want %v", p, got, want)
		}
	}
}

func zipWithEntries(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(data)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func contentTypes(mainTypes ...string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">`)
	for _, m := range mainTypes {
		b.WriteString(`<Override PartName="/x" ContentType="` + m + `"/>`)
	}
	b.WriteString(`</Types>`)
	return []byte(b.String())
}

func TestContentMimeOfOfficeContainersNeedsExactlyOneMainContentType(t *testing.T) {
	// An OOXML container is classified by its [Content_Types].xml: none, several, no such part, a huge one, too many entries or a broken zip are not documents.
	word := mimeDOCX + ".main+xml"
	sheet := mimeXLSX + ".main+xml"
	many := map[string][]byte{}
	for i := range 5001 {
		many[fmt.Sprintf("part-%d.xml", i)] = nil
	}
	many["[Content_Types].xml"] = contentTypes(word)
	cases := []struct {
		name  string
		data  []byte
		valid bool
	}{
		{"one main type", zipWithEntries(t, map[string][]byte{"[Content_Types].xml": contentTypes(word)}), true},
		{"two main types", zipWithEntries(t, map[string][]byte{"[Content_Types].xml": contentTypes(word, sheet)}), false},
		{"no main type", zipWithEntries(t, map[string][]byte{"[Content_Types].xml": contentTypes("text/plain")}), false},
		{"no content types part", zipWithEntries(t, map[string][]byte{"doc.xml": []byte("x")}), false},
		{"oversized content types part", zipWithEntries(t, map[string][]byte{"[Content_Types].xml": bytes.Repeat([]byte(" "), 1<<20+1)}), false},
		{"broken zip", []byte("PK\x03\x04 this is not a zip"), false},
		{"too many entries", zipWithEntries(t, many), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Act
			got := artifacts.ContentMime("blob.bin", c.data)

			// Assert
			if c.valid && got != mimeDOCX {
				t.Fatalf("ContentMime = %q, want the docx type", got)
			}
			if !c.valid && got == mimeDOCX {
				t.Fatalf("ContentMime = %q for an invalid container", got)
			}
		})
	}
}
