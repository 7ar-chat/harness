package store

import "context"

// HistoryStore is the optional part of a Store that edits a thread's history
// without starting a run (undo). The three stores implement it; a host
// checks with a type assertion.
type HistoryStore interface {
	// SupersedeFrom hides the user message messageID and every later message of the thread (like an edit
	// and resend, without the new run), drops the context summary that covered them and the pending
	// question. ErrNotFound when the thread is not login's, ErrInvalidReplace when messageID is not a
	// visible user message, ErrRunActive when the thread has a queued or running run.
	SupersedeFrom(ctx context.Context, login, threadID, messageID string) error
}
