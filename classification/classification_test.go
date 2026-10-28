package classification_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/classification"
)

type expectedFile struct {
	MimeType         string            `json:"mimeType"`
	Properties       map[string]string `json:"properties"`
	SourceProperties map[string]string `json:"sourceProperties"`
}

type expectedFixtures struct {
	Timestamp  string                  `json:"timestamp"`
	DocumentID string                  `json:"documentId"`
	ActionID   string                  `json:"actionId"`
	LineID     string                  `json:"lineId"`
	Actor      string                  `json:"actor"`
	Files      map[string]expectedFile `json:"files"`
}

func loadExpected(t *testing.T) expectedFixtures {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	var e expectedFixtures
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

// defaultSettings mirrors the ai/api/settings.py defaults used by the generator.
func defaultSettings(t *testing.T, mode string) classification.Settings {
	t.Helper()
	s, err := classification.Load(classification.Raw{
		Mode: mode, ProfileName: "c3", ProfileVersion: "2026-08", ProfileLevel: "C3",
		Providers:        []string{"mip", "gvdata"},
		ProviderSettings: classification.DefaultProviderSettings(func(_, def string) string { return def }),
		Actor:            "work",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fixedService labels with the generator's fixed clock and ids.
func fixedService(t *testing.T, e expectedFixtures) *classification.Service {
	t.Helper()
	svc, err := classification.New(defaultSettings(t, "enforce"))
	if err != nil {
		t.Fatal(err)
	}
	now, _ := time.Parse(time.RFC3339, e.Timestamp)
	ids := []uuid.UUID{uuid.MustParse(e.DocumentID), uuid.MustParse(e.ActionID), uuid.MustParse(e.LineID)}
	n := 0
	svc.Now = func() time.Time { return now }
	svc.NewUUID = func() uuid.UUID { id := ids[n%3]; n++; return id }
	return svc
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func properties(t *testing.T, name string, content []byte) map[string]string {
	t.Helper()
	var props map[string]string
	var err error
	if strings.HasSuffix(name, ".pdf") {
		props, err = classification.PDFInfo(content)
	} else {
		props, err = classification.ReadCustomProperties(content)
	}
	if err != nil {
		t.Fatalf("%s: read properties: %v", name, err)
	}
	return props
}

func diff(t *testing.T, label string, got, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: property %s = %q, want %q", label, k, got[k], v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: unexpected property %s = %q", label, k, got[k])
		}
	}
}

func zipEntry(t *testing.T, content []byte, name string) string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name == name {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			rc.Close()
			return string(b)
		}
	}
	return ""
}

func TestGoLabelsMatchThePythonImplementationPropertyForProperty(t *testing.T) {
	// Every fixture labelled by the Go port carries exactly the properties the Python service wrote.
	// Arrange
	e := loadExpected(t)
	if len(e.Files) == 0 {
		t.Fatal("no fixtures")
	}
	for name, want := range e.Files {
		t.Run(name, func(t *testing.T) {
			source := readFixture(t, name)
			svc := fixedService(t, e)

			// Act
			res, err := svc.Classify(context.Background(), name, want.MimeType, source, "work")

			// Assert
			if err != nil {
				t.Fatalf("classify: %v", err)
			}
			diff(t, "go", properties(t, name, res.Content), want.Properties)
			ext := filepath.Ext(name)
			pythonOut := readFixture(t, strings.TrimSuffix(name, ext)+".labelled"+ext)
			diff(t, "python (read by go)", properties(t, name, pythonOut), want.Properties)
			if ext != ".pdf" {
				if ct := zipEntry(t, res.Content, "[Content_Types].xml"); !strings.Contains(ct, `PartName="/docProps/custom.xml" ContentType="application/vnd.openxmlformats-officedocument.custom-properties+xml"`) {
					t.Errorf("content types lack the custom part override: %s", ct)
				}
				if rels := zipEntry(t, res.Content, "_rels/.rels"); !strings.Contains(rels, `Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/custom-properties" Target="docProps/custom.xml"`) {
					t.Errorf("package relationships lack the custom part: %s", rels)
				}
			}
		})
	}
}

func TestRelabellingALabelledDocumentReplacesItsLabel(t *testing.T) {
	// A document that already carries the provider properties is relabelled with the new ones replacing them (C-07), never doubled.
	// Arrange
	e := loadExpected(t)
	svc := fixedService(t, e)
	labelled := readFixture(t, "report.labelled.docx")

	// Act
	res, err := svc.Classify(context.Background(), "report.docx", "", labelled, "work")

	// Assert
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	diff(t, "relabelled", properties(t, "report.docx", res.Content), e.Files["report.docx"].Properties)
}

func TestDetectionRequiresFilenameMimeAndSignatureToAgree(t *testing.T) {
	// Supported formats are only detected when extension, MIME type and bytes agree.
	// Arrange
	docx := readFixture(t, "report.docx")
	pdf := readFixture(t, "memo.pdf")
	cases := []struct {
		name, filename, mime string
		content              []byte
		want                 classification.Format
		wantErr              error
	}{
		{"docx with generic mime", "a.docx", "application/octet-stream", docx, classification.DOCX, nil},
		{"pdf declared", "a.pdf", "application/pdf", pdf, classification.PDF, nil},
		{"pdf bytes named docx", "a.docx", "", pdf, "", classification.ErrFormatMismatch},
		{"pdf mime on docx bytes", "a.docx", "application/pdf", docx, "", classification.ErrFormatMismatch},
		{"text named pdf", "a.pdf", "application/pdf", []byte("hello"), "", classification.ErrInvalidDocument},
		{"pdf bytes without extension", "a.bin", "", pdf, "", classification.ErrFormatMismatch},
		{"unsupported text", "notes.txt", "text/plain", []byte("hello"), "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Act
			got, err := classification.Detect(c.filename, c.mime, c.content)

			// Assert
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func TestBlockModeRefusesSupportedDocuments(t *testing.T) {
	// In block mode supported documents are never delivered.
	// Arrange
	svc, err := classification.New(defaultSettings(t, "block"))
	if err != nil {
		t.Fatal(err)
	}

	// Act
	_, err = svc.Classify(context.Background(), "memo.pdf", "application/pdf", readFixture(t, "memo.pdf"), "")

	// Assert
	if !errors.Is(err, classification.ErrUnsupportedFormat) {
		t.Fatalf("err = %v, want ErrUnsupportedFormat", err)
	}
}

func TestSettingsValidationRejectsUnknownProvidersAndModes(t *testing.T) {
	// Invalid deployment configuration is a configuration error.
	// Arrange
	base := classification.Raw{Mode: "enforce", ProfileName: "c3", ProfileVersion: "v", ProfileLevel: "C3",
		Providers: []string{"mip"}, ProviderSettings: classification.DefaultProviderSettings(func(_, d string) string { return d })}
	badMode, unknown, noEnabled := base, base, base
	badMode.Mode = "audit"
	unknown.Providers = []string{"mip", "acme"}
	noEnabled.ProviderSettings = map[string]map[string]any{"mip": {"label_id": "x"}}

	for name, raw := range map[string]classification.Raw{"mode": badMode, "unknown": unknown, "enabled": noEnabled} {
		// Act
		_, err := classification.Load(raw)

		// Assert
		if !errors.Is(err, classification.ErrConfiguration) {
			t.Errorf("%s: err = %v, want ErrConfiguration", name, err)
		}
	}
}

func TestConfidentialityLabelsAreReadFromOfficeAndPDF(t *testing.T) {
	// C4 / C3-Amundi-Only labels are found in OOXML custom properties and in the PDF header lines.
	// Arrange
	e := loadExpected(t)
	labelled := readFixture(t, "report.labelled.docx")
	pdfC4 := []byte("%PDF-1.4\n1 0 obj\n<< /MSIP_Label_abc_Name(C4 - Secret) >>\nendobj\n")

	// Act
	studioLabel := classification.ConfidentialityLabel("report.docx", labelled)
	pdfLabel := classification.ConfidentialityLabel("x.pdf", pdfC4)
	plain := classification.ConfidentialityLabel("report.docx", readFixture(t, "report.docx"))

	// Assert
	if studioLabel != "C3-External-Allowed" || classification.ConfidentialityDisallowed(studioLabel) {
		t.Errorf("studio label %q", studioLabel)
	}
	if pdfLabel != "C4 - Secret" || !classification.ConfidentialityDisallowed(pdfLabel) {
		t.Errorf("pdf label %q", pdfLabel)
	}
	if plain != "" {
		t.Errorf("unlabelled docx has label %q", plain)
	}
	if !classification.ConfidentialityDisallowed("C3-Amundi-Only") || classification.ConfidentialityDisallowed("C2") {
		t.Error("disallowed rule")
	}
	_ = e
}

func TestClassifyStopsWaitingForASlotWhenItsContextEnds(t *testing.T) {
	// With every classification slot busy, a waiting Classify returns its context's error instead of blocking (review round 1).
	// Arrange
	settings := defaultSettings(t, "enforce")
	settings.MaxConcurrent = 1
	svc, err := classification.New(settings)
	if err != nil {
		t.Fatal(err)
	}
	inside, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	svc.NewUUID = func() uuid.UUID {
		once.Do(func() { close(inside); <-release }) // the first classification holds the only slot
		return uuid.New()
	}
	go func() {
		_, _ = svc.Classify(context.Background(), "memo.pdf", "application/pdf", readFixture(t, "memo.pdf"), "")
	}()
	<-inside
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)

	// Act
	go func() {
		_, err := svc.Classify(ctx, "memo.pdf", "application/pdf", readFixture(t, "memo.pdf"), "")
		done <- err
	}()

	// Assert
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, classification.ErrClassification) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Classify still waits for a slot after its context ended")
	}
}
