package artifacts

import (
	"mime"
	"path"
	"strings"

	"github.com/GhaziBenDahmane/harness/classification"
)

var extraTypes = map[string]string{
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".doc":  "application/msword",
	".xls":  "application/vnd.ms-excel",
	".ppt":  "application/vnd.ms-powerpoint",
	".odt":  "application/vnd.oasis.opendocument.text",
	".ods":  "application/vnd.oasis.opendocument.spreadsheet",
	".odp":  "application/vnd.oasis.opendocument.presentation",
	".rtf":  "application/rtf",
	".pdf":  "application/pdf",
	".csv":  "text/csv",
	".tsv":  "text/tab-separated-values",
	".md":   "text/markdown",
	".txt":  "text/plain",
	".log":  "text/plain",
	".json": "application/json",
	".yaml": "application/yaml",
	".yml":  "application/yaml",
	".xml":  "application/xml",
	".html": "text/html",
	".htm":  "text/html",
	".py":   "text/x-python",
	".js":   "text/javascript",
	".ts":   "text/plain",
	".sh":   "text/x-shellscript",
	".sql":  "text/plain",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".svg":  "image/svg+xml",
	".zip":  "application/zip",
}

// MimeType is the content type of a file name (application/octet-stream when unknown).
func MimeType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if t, ok := extraTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		t, _, _ = strings.Cut(t, ";")
		return t
	}
	return "application/octet-stream"
}

var officeExts = map[string]bool{".docx": true, ".xlsx": true, ".pptx": true, ".doc": true, ".xls": true, ".ppt": true,
	".odt": true, ".ods": true, ".odp": true, ".rtf": true}

var textExts = map[string]bool{".txt": true, ".md": true, ".csv": true, ".tsv": true, ".json": true, ".yaml": true,
	".yml": true, ".xml": true, ".py": true, ".js": true, ".ts": true, ".tsx": true, ".jsx": true, ".sh": true, ".sql": true,
	".log": true, ".ini": true, ".toml": true, ".css": true, ".r": true, ".go": true, ".java": true, ".c": true, ".h": true,
	".cpp": true, ".rs": true, ".tex": true, ".mjs": true}

var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".svg": true}

// Previewable is the preview kind of a file: pdf (PDF and Office files, converted), image, text, html or none.
func Previewable(name string) string {
	ext := strings.ToLower(path.Ext(name))
	switch {
	case ext == ".pdf" || officeExts[ext]:
		return "pdf"
	case imageExts[ext]:
		return "image"
	case ext == ".html" || ext == ".htm":
		return "html"
	case textExts[ext]:
		return "text"
	}
	return "none"
}

// ContentMime is the content type recorded for a file: the extension's type,
// unless the bytes carry a PDF/Office signature the extension does not claim
// (e.g. a PDF renamed memo.txt), then the signature's type (or
// application/octet-stream when the signature is a broken one).
func ContentMime(name string, data []byte) string {
	byName := MimeType(name)
	if classification.FormatForFilename(name) != "" || !classification.RequiresClassification(name, byName, data) {
		return byName
	}
	if f := classification.Signature(data); f != "" {
		return f.ContentType()
	}
	return "application/octet-stream"
}

// PreviewableAs is Previewable for a file recorded with mimeType: a file whose
// content type (from its signature, see ContentMime) is not its extension's
// is never previewed.
func PreviewableAs(name, mimeType string) string {
	if mimeType != "" && mimeType != MimeType(name) && classifiedMime(mimeType) {
		return "none"
	}
	return Previewable(name)
}

func classifiedMime(mt string) bool {
	switch mt {
	case "application/octet-stream":
		return true
	}
	for _, f := range []classification.Format{classification.PDF, classification.DOCX, classification.XLSX, classification.PPTX} {
		if f.ContentType() == mt {
			return true
		}
	}
	return false
}

func isOffice(name string) bool { return officeExts[strings.ToLower(path.Ext(name))] }
