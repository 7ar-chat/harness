package harness

import "github.com/7ar-chat/harness/features/documents"

// The user's document library (Harness.Documents), as the HTTP product sees it.
type (
	// Document describes one stored document.
	Document = documents.Doc
)

var (
	// ErrDocumentInvalid is a document name or content that is refused.
	ErrDocumentInvalid = documents.ErrInvalid
	// ErrDocumentNotFound means the user has no such document.
	ErrDocumentNotFound = documents.ErrNotFound
	// ErrDocumentLimit means the user is over the number or total size of documents.
	ErrDocumentLimit = documents.ErrLimit
)

// MaxDocumentBytes is the largest single document.
const MaxDocumentBytes = documents.MaxFileBytes
