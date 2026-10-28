// Package classification ports ai/file_classification (Studio Work document
// classification) to Go: fail-closed detection (filename extension, declared
// MIME type and byte signature must agree), OOXML custom-property labels
// (docProps/custom.xml + content types + package relationships), PDF document
// information labels, and the MIP / GVData metadata providers.
package classification

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Format is a supported document container format.
type Format string

// Supported formats.
const (
	PDF  Format = "pdf"
	DOCX Format = "docx"
	XLSX Format = "xlsx"
	PPTX Format = "pptx"
)

// ContentType is the delivery MIME type of a format.
func (f Format) ContentType() string {
	switch f {
	case PDF:
		return "application/pdf"
	case DOCX:
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case XLSX:
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case PPTX:
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	}
	return "application/octet-stream"
}

// Error categories (mirror ai/file_classification/errors.py). Every failure
// wraps exactly one of them, so callers can fail closed with errors.Is.
var (
	ErrClassification     = errors.New("classification failed")
	ErrUnsupportedFormat  = fmt.Errorf("%w: unsupported document format", ErrClassification)
	ErrFormatMismatch     = fmt.Errorf("%w: filename, MIME type and signature disagree", ErrClassification)
	ErrInvalidDocument    = fmt.Errorf("%w: invalid document", ErrClassification)
	ErrSizeExceeded       = fmt.Errorf("%w: size limit exceeded", ErrClassification)
	ErrConfiguration      = fmt.Errorf("%w: configuration error", ErrClassification)
	ErrProvider           = fmt.Errorf("%w: provider error", ErrClassification)
	ErrVerificationFailed = fmt.Errorf("%w: verification failed", ErrClassification)
	// ErrSignedDocument is a digitally signed OOXML package (refused: labelling
	// would break the signature; C-27). It is an ErrInvalidDocument.
	ErrSignedDocument = fmt.Errorf("%w: signed document", ErrInvalidDocument)
)

func errf(kind error, format string, args ...any) error {
	return fmt.Errorf("%w: %s", kind, fmt.Sprintf(format, args...))
}

// Profile is the deployment-selected classification profile.
type Profile struct {
	Name      string
	Version   string
	Level     string
	Providers []string
	// ProviderSettings maps provider name → setting → value (all text, "enabled" normalized to true/false).
	ProviderSettings map[string]map[string]string
}

// Settings is the validated deployment configuration.
type Settings struct {
	// Mode is "enforce" (label supported documents) or "block" (refuse them).
	Mode           string
	Profile        Profile
	Actor          string
	MaxInputBytes  int64
	MaxOutputBytes int64
	// MaxConcurrent bounds concurrent classifications per process
	// (FILE_CLASSIFICATION_MAX_CONCURRENT, default 4).
	MaxConcurrent int
}

// Context holds the identifiers shared by every provider for one operation.
type Context struct {
	Timestamp      time.Time
	DocumentID     uuid.UUID
	ActionID       uuid.UUID
	LineID         uuid.UUID
	Actor          string
	ProfileName    string
	ProfileVersion string
}

// Result is a classified document.
type Result struct {
	Content   []byte
	Format    Format
	Context   Context
	Providers []string
	// Properties are the metadata written (keys without the PDF "/" prefix).
	Properties map[string]string
}
