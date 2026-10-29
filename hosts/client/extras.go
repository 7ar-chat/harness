package client

import "context"

// Extras are the operations beyond the core Client contract. A front end checks
// for them with a type assertion (c.(client.Extras)); an implementation or a
// server that lacks one answers ErrNotSupported, so a newer front end still
// works against an older server. Both InProcess and HTTP implement it.
type Extras interface {
	// Compact summarises the thread's history now (the automatic compaction runs by itself near the context limit).
	Compact(ctx context.Context, threadID string) (*CompactResult, error)
	// Undo removes the thread's last user message with the answers after it and restores the workspace
	// files to the snapshot taken before that run (a git workspace). The removed prompt is returned so the
	// UI can put it back in the input. Repeated calls walk back through the thread.
	Undo(ctx context.Context, threadID string) (*UndoResult, error)
	// Redo reapplies the last undone step (only after Undo, until a new message is sent).
	Redo(ctx context.Context, threadID string) (*UndoResult, error)
	// Fork copies the thread up to and including messageID ("" is the whole thread) into a new thread.
	Fork(ctx context.Context, threadID, messageID string) (*Thread, error)
	// Commands lists the custom commands (markdown prompt templates) and skills a user can invoke by name.
	Commands(ctx context.Context) ([]Command, error)
	// ExpandCommand returns the prompt of "/name args" (the template with $ARGUMENTS and $1.. replaced).
	ExpandCommand(ctx context.Context, name, args string) (string, error)
}

// CompactResult is the outcome of Compact.
type CompactResult struct {
	// Compacted is false when there was nothing to summarise.
	Compacted    bool `json:"compacted"`
	TokensBefore int  `json:"tokensBefore,omitempty"`
	TokensAfter  int  `json:"tokensAfter,omitempty"`
}

// UndoResult is the outcome of Undo and Redo.
type UndoResult struct {
	// Prompt is the user message that was removed (Undo) or restored (Redo).
	Prompt string `json:"prompt"`
	// Files are the workspace paths whose content changed; empty when the workspace is not a git repository.
	Files []string `json:"files,omitempty"`
	// CanUndo and CanRedo tell whether another step is available.
	CanUndo bool `json:"canUndo"`
	CanRedo bool `json:"canRedo"`
}

// Command is a custom command or a skill invocable as /name.
type Command struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Source is "project", "user", "skill" or "builtin".
	Source string `json:"source,omitempty"`
	// ArgHint is shown after the name, e.g. "<file>".
	ArgHint string `json:"argHint,omitempty"`
}

// Run modes of SendOptions.Mode.
const (
	// ModeBuild is the default: all tools the profile allows.
	ModeBuild = "build"
	// ModePlan is read-only: the agent investigates and proposes a plan, and no tool may change files or run mutating commands.
	ModePlan = "plan"
)

// Attachment is a file sent along with a message (an image pasted into the UI).
type Attachment struct {
	Name string `json:"name"`
	// MIME is the media type, e.g. image/png.
	MIME string `json:"mime"`
	// Data is the content (base64 on the wire).
	Data []byte `json:"data"`
}

var (
	_ Extras = (*inProcess)(nil)
	_ Extras = (*httpClient)(nil)
)
