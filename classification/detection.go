package classification

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"path"
	"strings"
)

var mimeToFormat = map[string]Format{
	"application/pdf": PDF,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   DOCX,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         XLSX,
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": PPTX,
}

var extensionToFormat = map[string]Format{".pdf": PDF, ".docx": DOCX, ".xlsx": XLSX, ".pptx": PPTX}

var ooxmlMainContentTypes = map[string]Format{
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml":   DOCX,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml":         XLSX,
	"application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml": PPTX,
}

var oleHeader = []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}

// FormatForFilename returns the supported format of a filename extension ("" when unsupported).
func FormatForFilename(filename string) Format {
	return extensionToFormat[strings.ToLower(path.Ext(strings.ReplaceAll(filename, "\\", "/")))]
}

func normalizeMIME(contentType string) string {
	ct, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(ct))
}

func ooxmlSignature(content []byte) (Format, error) {
	if bytes.HasPrefix(content, oleHeader) {
		// Legacy OLE documents (DOC/XLS/PPT, RMS-protected OOXML) are deliberately unsupported.
		return "", nil
	}
	if !bytes.HasPrefix(content, []byte("PK")) {
		return "", nil
	}
	zr, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return "", nil
	}
	if len(zr.File) > maxZipEntries {
		return "", errf(ErrInvalidDocument, "OOXML package has too many entries")
	}
	var total uint64
	var contentTypes *zip.File
	lowered := map[string]bool{}
	for _, f := range zr.File {
		total += f.UncompressedSize64
		if total > maxUncompressedBytes {
			return "", errf(ErrInvalidDocument, "OOXML package is too large")
		}
		lowered[strings.ToLower(f.Name)] = true
		if f.Name == contentTypesPart {
			contentTypes = f
		}
	}
	for _, f := range zr.File {
		if f.Flags&0x1 != 0 {
			return "", errf(ErrInvalidDocument, "encrypted OOXML package")
		}
	}
	for name := range lowered {
		if name == "encryptioninfo" || name == "encryptedpackage" || strings.Contains(name, "_xmlsignatures/") || strings.HasPrefix(name, "_xmlsignatures") {
			if lowered["encryptioninfo"] || lowered["encryptedpackage"] {
				return "", errf(ErrInvalidDocument, "encrypted OOXML package")
			}
			return "", errf(ErrSignedDocument, "signed OOXML package")
		}
	}
	if err := checkExpansion(total, len(content)); err != nil {
		return "", err
	}
	if contentTypes == nil {
		return "", nil
	}
	raw, err := readEntryBounded(contentTypes, maxContentTypesBytes)
	if err != nil {
		if isZipFormatError(err) {
			return "", nil
		}
		return "", err
	}
	formats := map[Format]bool{}
	dec := xml.NewDecoder(bytes.NewReader(raw))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", errf(ErrInvalidDocument, "invalid OOXML content types")
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
	if len(formats) > 1 {
		return "", errf(ErrInvalidDocument, "OOXML package contains multiple document subtypes")
	}
	for f := range formats {
		return f, nil
	}
	return "", nil
}

// Signature is the supported format of a byte signature ("" when none or invalid).
func Signature(content []byte) Format {
	f, err := signature(content)
	if err != nil {
		return ""
	}
	return f
}

func signature(content []byte) (Format, error) {
	if bytes.HasPrefix(content, []byte("%PDF-")) {
		return PDF, nil
	}
	return ooxmlSignature(content)
}

// Detect returns the supported format only when every supplied indicator
// (filename extension, declared MIME type, byte signature) agrees, like
// detect_document_format. "" (nil error) means genuinely unsupported input; a
// supported indicator with a missing or conflicting one is an error (fail closed).
// A blank or application/octet-stream MIME type is non-declarative.
func Detect(filename, contentType string, content []byte) (Format, error) {
	extFormat := FormatForFilename(filename)
	mime := normalizeMIME(contentType)
	nonDeclarative := mime == "" || mime == "application/octet-stream"
	var mimeFormat Format
	if !nonDeclarative {
		mimeFormat = mimeToFormat[mime]
	}
	sigFormat, err := signature(content)
	if err != nil {
		return "", err
	}
	var indicators []Format
	for _, f := range []Format{extFormat, mimeFormat, sigFormat} {
		if f != "" {
			indicators = append(indicators, f)
		}
	}
	if len(indicators) == 0 {
		return "", nil
	}
	for _, f := range indicators[1:] {
		if f != indicators[0] {
			return "", errf(ErrFormatMismatch, "filename extension, MIME type, and signature disagree")
		}
	}
	if mimeFormat == "" && !nonDeclarative && (extFormat != "" || sigFormat != "") {
		return "", errf(ErrFormatMismatch, "filename extension, MIME type, and signature disagree")
	}
	if extFormat == "" || sigFormat == "" {
		if extFormat != "" && mimeFormat != "" {
			return "", errf(ErrInvalidDocument, "supported document has an invalid signature")
		}
		return "", errf(ErrFormatMismatch, "supported filename, MIME type, and signature must all agree")
	}
	return indicators[0], nil
}

// RequiresClassification mirrors Cowork's delivery rule: a file is classified
// when its extension or MIME type is a supported one, or when its bytes are
// detected as (or conflict with) a supported format.
func RequiresClassification(filename, contentType string, content []byte) bool {
	if FormatForFilename(filename) != "" || mimeToFormat[normalizeMIME(contentType)] != "" {
		return true
	}
	f, err := Detect(filename, contentType, content)
	return err != nil || f != ""
}
