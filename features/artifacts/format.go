package artifacts

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"path"
	"strings"
)

// docFormat is a document format the upload checks recognise by signature.
type docFormat string

const (
	fmtPDF  docFormat = "pdf"
	fmtDOCX docFormat = "docx"
	fmtXLSX docFormat = "xlsx"
	fmtPPTX docFormat = "pptx"
)

func (f docFormat) contentType() string {
	switch f {
	case fmtPDF:
		return "application/pdf"
	case fmtDOCX:
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case fmtXLSX:
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case fmtPPTX:
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	}
	return "application/octet-stream"
}

var mimeToFormat = map[string]docFormat{
	"application/pdf": fmtPDF,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   fmtDOCX,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         fmtXLSX,
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": fmtPPTX,
}

var extensionToFormat = map[string]docFormat{".pdf": fmtPDF, ".docx": fmtDOCX, ".xlsx": fmtXLSX, ".pptx": fmtPPTX}

var ooxmlMainContentTypes = map[string]docFormat{
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml":   fmtDOCX,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml":         fmtXLSX,
	"application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml": fmtPPTX,
}

const (
	maxZipEntries        = 5000
	maxContentTypesBytes = 1 << 20
)

func formatForFilename(filename string) docFormat {
	return extensionToFormat[strings.ToLower(path.Ext(strings.ReplaceAll(filename, "\\", "/")))]
}

// signature is the document format of the bytes ("" when none or invalid).
func signature(content []byte) docFormat {
	if bytes.HasPrefix(content, []byte("%PDF-")) {
		return fmtPDF
	}
	if !bytes.HasPrefix(content, []byte("PK")) {
		return ""
	}
	zr, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil || len(zr.File) > maxZipEntries {
		return ""
	}
	for _, f := range zr.File {
		if f.Name != "[Content_Types].xml" || f.UncompressedSize64 > maxContentTypesBytes {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return ""
		}
		raw, err := io.ReadAll(io.LimitReader(rc, maxContentTypesBytes))
		rc.Close()
		if err != nil {
			return ""
		}
		formats := map[docFormat]bool{}
		dec := xml.NewDecoder(bytes.NewReader(raw))
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			if se, ok := tok.(xml.StartElement); ok {
				for _, a := range se.Attr {
					if a.Name.Local == "ContentType" && a.Name.Space == "" {
						if f, ok := ooxmlMainContentTypes[a.Value]; ok {
							formats[f] = true
						}
					}
				}
			}
		}
		if len(formats) != 1 {
			return ""
		}
		for f := range formats {
			return f
		}
	}
	return ""
}

// detectFormat returns the document format only when every supplied indicator
// (filename extension, declared MIME type, byte signature) agrees. "" with ok
// means input that is not a supported document; !ok means the indicators
// disagree or a supported name/type carries no valid signature.
func detectFormat(filename, contentType string, content []byte) (docFormat, bool) {
	extFormat := formatForFilename(filename)
	mime := normalizeMIME(contentType)
	nonDeclarative := mime == "" || mime == "application/octet-stream"
	var mimeFormat docFormat
	if !nonDeclarative {
		mimeFormat = mimeToFormat[mime]
	}
	sigFormat := signature(content)
	var indicators []docFormat
	for _, f := range []docFormat{extFormat, mimeFormat, sigFormat} {
		if f != "" {
			indicators = append(indicators, f)
		}
	}
	if len(indicators) == 0 {
		return "", true
	}
	for _, f := range indicators[1:] {
		if f != indicators[0] {
			return "", false
		}
	}
	if mimeFormat == "" && !nonDeclarative && (extFormat != "" || sigFormat != "") {
		return "", false
	}
	if extFormat == "" || sigFormat == "" {
		return "", false
	}
	return indicators[0], true
}

// isDocument reports a file whose extension or MIME type is a supported
// document one, or whose bytes are (or conflict with) a supported format.
func isDocument(filename, contentType string, content []byte) bool {
	if formatForFilename(filename) != "" || mimeToFormat[normalizeMIME(contentType)] != "" {
		return true
	}
	f, ok := detectFormat(filename, contentType, content)
	return !ok || f != ""
}

// needsWholeContent reports bytes whose type checks need the whole content: a
// PDF or zip header, or a zip end-of-central-directory record (a zip with a prefix).
func needsWholeContent(head, tail []byte) bool {
	return bytes.HasPrefix(head, []byte("%PDF-")) || bytes.HasPrefix(head, []byte("PK")) ||
		bytes.Contains(tail, []byte("PK\x05\x06"))
}
