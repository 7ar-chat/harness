package ideas_test

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/classification"
	"github.com/GhaziBenDahmane/harness/ideas"
)

func TestCatalogHasWorksTenIdeasInOrder(t *testing.T) {
	// The catalog is Work's: ten ideas in display order, with their categories and demo files.
	// Arrange
	want := []struct{ id, title, category string }{
		{"generate-presentation", "Generate a presentation", "Office"},
		{"analyze-excel", "Analyze a spreadsheet", "Data"},
		{"generate-word-document", "Generate a Word document", "Office"},
		{"summarize-pdf", "Summarize a PDF", "Legal"},
		{"create-kpi-report", "Create a KPI report", "Reporting"},
		{"draft-client-email", "Draft a professional email", "Communication"},
		{"clean-csv-data", "Clean and restructure data", "Data"},
		{"create-project-plan", "Create a project plan", "Planning"},
		{"compare-documents", "Compare documents", "Review"},
		{"generate-faq", "Generate an FAQ", "Knowledge"},
	}

	// Act
	got := ideas.All(false)

	// Assert
	if len(got) != len(want) {
		t.Fatalf("got %d ideas", len(got))
	}
	for i, w := range want {
		g := got[i]
		if g.ID != w.id || g.Title != w.title || g.Category != w.category || g.Summary == "" || g.Prompt == "" || len(g.FakeFiles) == 0 {
			t.Errorf("idea %d = %+v, want %+v", i, g, w)
		}
	}
}

func TestCatalogFeaturedOnHomeKeepsTheFirstFour(t *testing.T) {
	// Like Work, presentation, spreadsheet, Word document and PDF summary are featured on the home page.
	// Act
	got := ideas.All(true)

	// Assert
	var ids []string
	for _, i := range got {
		ids = append(ids, i.ID)
	}
	if strings.Join(ids, ",") != "generate-presentation,analyze-excel,generate-word-document,summarize-pdf" {
		t.Fatalf("featured = %v", ids)
	}
}

func TestCatalogCopiesWorksPromptsAndDemoContent(t *testing.T) {
	// Prompts, demo file names, content types and contents are Work's (the presentation prompt asks for a .pptx).
	// Act
	pres, ok := ideas.Get(" generate-presentation ")
	pdf, _ := ideas.Get("summarize-pdf")
	_, unknown := ideas.Get("nope")

	// Assert
	if !ok || unknown {
		t.Fatalf("Get: %v %v", ok, unknown)
	}
	if pres.Prompt != "Create an 8-slide PowerPoint presentation as a .pptx file from the demo file product_launch_brief.md. Include context, key messages, launch plan, risks, and next steps." {
		t.Fatalf("prompt = %q", pres.Prompt)
	}
	files := pres.DemoFiles()
	if len(files) != 2 || files[0].Name != "product_launch_brief.md" || files[0].ContentType != ideas.Markdown ||
		!strings.HasPrefix(files[0].Content, "# Product launch brief\nProduct: Astra Assist Teams") || string(files[0].Data) != files[0].Content ||
		files[1].Name != "market_demo.csv" || files[1].ContentType != ideas.CSV {
		t.Fatalf("files = %+v", files)
	}
	if d := pdf.DemoFiles()[0]; d.Name != "supplier_contract_demo.pdf" || d.ContentType != ideas.PDF || !strings.Contains(d.Content, "SLA: 99.5% availability") {
		t.Fatalf("pdf = %+v", d)
	}
	if pres.RecommendedSkillSlugs[0] != "anthropic-pptx" {
		t.Fatalf("skills = %v", pres.RecommendedSkillSlugs)
	}
}

func TestCatalogEveryDemoFileIsKnownAndUploadable(t *testing.T) {
	// Every fake file has Work's content; PDF and Office demo files are real documents whose
	// extension, MIME type and signature agree (the upload check), text files are their content.
	for _, idea := range ideas.All(false) {
		for _, f := range idea.DemoFiles() {
			t.Run(idea.ID+"/"+f.Name, func(t *testing.T) {
				// Act
				format, err := classification.Detect(f.Name, f.ContentType, f.Data)

				// Assert
				if strings.HasPrefix(f.Content, "Demo file: ") {
					t.Fatalf("no demo content for %s", f.Name)
				}
				if err != nil {
					t.Fatalf("detect: %v", err)
				}
				if f.Binary() != (format != "") {
					t.Fatalf("binary %v, format %q", f.Binary(), format)
				}
				if !f.Binary() && string(f.Data) != f.Content {
					t.Fatal("text data differs from content")
				}
			})
		}
	}
}

func zipEntry(t *testing.T, data []byte, name string) string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name == name {
			r, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(r)
			return string(b)
		}
	}
	t.Fatalf("no %s", name)
	return ""
}

func TestCatalogGeneratedDocumentsHoldTheDemoText(t *testing.T) {
	// The generated workbook, Word documents and PDF contain Work's demo text (numbers as numeric cells).
	// Arrange
	xlsx, _ := ideas.Get("analyze-excel")
	docx, _ := ideas.Get("compare-documents")
	pdf, _ := ideas.Get("summarize-pdf")

	// Act
	sheet := zipEntry(t, xlsx.DemoFiles()[0].Data, "xl/worksheets/sheet1.xml")
	doc := zipEntry(t, docx.DemoFiles()[1].Data, "word/document.xml")
	info, err := classification.PDFInfo(pdf.DemoFiles()[0].Data)

	// Assert
	if !strings.Contains(sheet, `<c r="A1" t="inlineStr"><is><t xml:space="preserve">Region</t></is></c>`) || !strings.Contains(sheet, `<c r="E9"><v>33000</v></c>`) {
		t.Fatalf("sheet = %s", sheet)
	}
	if !strings.Contains(doc, "Remote work policy V2") || !strings.Contains(doc, "MFA enabled.") {
		t.Fatalf("doc = %s", doc)
	}
	if err != nil || info["/Title"] != "Demo supplier contract" {
		t.Fatalf("pdf info %v %v", info, err)
	}
	if !bytes.Contains(pdf.DemoFiles()[0].Data, []byte("(Negotiation point: annual security audit and liability cap.) Tj")) {
		t.Fatal("pdf misses the text")
	}
}
