package ideas

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The demo PDF and Office files are generated from Work's inline text so they
// are real documents (their signature matches their name, as uploads require).

// zipModified keeps the generated packages byte-for-byte stable.
var zipModified = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func xmlEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

type part struct{ name, body string }

func zipPackage(parts []part) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range parts {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: p.name, Method: zip.Deflate, Modified: zipModified})
		if err != nil {
			panic(err) // writing to memory cannot fail
		}
		if _, err := w.Write([]byte(p.body)); err != nil {
			panic(err)
		}
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

const (
	xmlHeader    = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n"
	relsType     = "application/vnd.openxmlformats-package.relationships+xml"
	officeDocRel = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument"
)

func contentTypes(overrides ...[2]string) string {
	var b strings.Builder
	b.WriteString(xmlHeader + `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">`)
	b.WriteString(`<Default Extension="rels" ContentType="` + relsType + `"/><Default Extension="xml" ContentType="application/xml"/>`)
	for _, o := range overrides {
		b.WriteString(`<Override PartName="` + o[0] + `" ContentType="` + o[1] + `"/>`)
	}
	b.WriteString(`</Types>`)
	return b.String()
}

func relationships(rels ...[3]string) string {
	var b strings.Builder
	b.WriteString(xmlHeader + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for _, r := range rels {
		b.WriteString(`<Relationship Id="` + r[0] + `" Type="` + r[1] + `" Target="` + r[2] + `"/>`)
	}
	b.WriteString(`</Relationships>`)
	return b.String()
}

// docxDocument is a Word document: the first line as a bold title, then one paragraph per line.
func docxDocument(text string) []byte {
	var body strings.Builder
	for i, line := range strings.Split(text, "\n") {
		props := ""
		if i == 0 {
			props = `<w:rPr><w:b/><w:sz w:val="32"/></w:rPr>`
		}
		body.WriteString(`<w:p><w:r>` + props + `<w:t xml:space="preserve">` + xmlEscape(line) + `</w:t></w:r></w:p>`)
	}
	doc := xmlHeader + `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` +
		body.String() + `<w:sectPr/></w:body></w:document>`
	return zipPackage([]part{
		{"[Content_Types].xml", contentTypes([2]string{"/word/document.xml", "application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"})},
		{"_rels/.rels", relationships([3]string{"rId1", officeDocRel, "word/document.xml"})},
		{"word/document.xml", doc},
	})
}

func columnName(i int) string {
	name := ""
	for i++; i > 0; i = (i - 1) / 26 {
		name = string(rune('A'+(i-1)%26)) + name
	}
	return name
}

// xlsxDocument is a one-sheet workbook of CSV text (numbers as numeric cells).
func xlsxDocument(csvText string) []byte {
	var rows strings.Builder
	for r, line := range strings.Split(csvText, "\n") {
		fmt.Fprintf(&rows, `<row r="%d">`, r+1)
		for c, cell := range strings.Split(line, ",") {
			ref := columnName(c) + strconv.Itoa(r+1)
			if _, err := strconv.ParseFloat(cell, 64); err == nil && r > 0 {
				rows.WriteString(`<c r="` + ref + `"><v>` + cell + `</v></c>`)
				continue
			}
			rows.WriteString(`<c r="` + ref + `" t="inlineStr"><is><t xml:space="preserve">` + xmlEscape(cell) + `</t></is></c>`)
		}
		rows.WriteString(`</row>`)
	}
	const ns = `xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"`
	return zipPackage([]part{
		{"[Content_Types].xml", contentTypes(
			[2]string{"/xl/workbook.xml", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"},
			[2]string{"/xl/worksheets/sheet1.xml", "application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"},
		)},
		{"_rels/.rels", relationships([3]string{"rId1", officeDocRel, "xl/workbook.xml"})},
		{"xl/workbook.xml", xmlHeader + `<workbook ` + ns + ` xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
			`<sheets><sheet name="Sheet1" sheetId="1" r:id="rId1"/></sheets></workbook>`},
		{"xl/_rels/workbook.xml.rels", relationships([3]string{"rId1", "http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet", "worksheets/sheet1.xml"})},
		{"xl/worksheets/sheet1.xml", xmlHeader + `<worksheet ` + ns + `><sheetData>` + rows.String() + `</sheetData></worksheet>`},
	})
}

func pdfString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`)
	return "(" + r.Replace(s) + ")"
}

// pdfDocument is a one-page A4 PDF: the first line as a bold title, then the other lines.
func pdfDocument(text string) []byte {
	all := strings.Split(text, "\n")
	var content strings.Builder
	content.WriteString("BT /F1 16 Tf 56 780 Td " + pdfString(all[0]) + " Tj ET\n")
	content.WriteString("BT /F2 11 Tf 16 TL 56 750 Td")
	for i, line := range all[1:] {
		if i > 0 {
			content.WriteString(" T*")
		}
		content.WriteString(" " + pdfString(line) + " Tj")
	}
	content.WriteString(" ET\n")
	stream := content.String()
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Resources << /Font << /F1 4 0 R /F2 5 0 R >> >> /Contents 6 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
		"<< /Length " + strconv.Itoa(len(stream)) + " >>\nstream\n" + stream + "endstream",
		"<< /Title " + pdfString(all[0]) + " /Producer (Work Chat) >>",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	offsets := make([]int, len(objects))
	for i, obj := range objects {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R /Info %d 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, len(objects), xref)
	return b.Bytes()
}
