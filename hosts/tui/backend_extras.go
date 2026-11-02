package tui

import (
	"context"

	"github.com/7ar-chat/harness/hosts/client"
)

// Optional capabilities of a Backend, each checked by type assertion so a
// backend (or a server) without one degrades to a notice. ClientBackend
// implements them over client.Extras.

// HistoryBackend edits the conversation: compact, undo/redo, fork.
type HistoryBackend interface {
	Compact(ctx context.Context) (*client.CompactResult, error)
	Undo(ctx context.Context) (*client.UndoResult, error)
	Redo(ctx context.Context) (*client.UndoResult, error)
	// Fork copies the current thread up to messageID ("" all of it) and switches to the copy.
	Fork(ctx context.Context, messageID string) (*client.Thread, error)
}

// CommandBackend lists and expands custom commands and skills.
type CommandBackend interface {
	Commands(ctx context.Context) ([]client.Command, error)
	ExpandCommand(ctx context.Context, name, args string) (string, error)
}

// RunOptionsBackend sets the per-run options: plan/build mode, reasoning effort, attachments for the next message.
type RunOptionsBackend interface {
	SetMode(mode string) // client.ModePlan or client.ModeBuild
	Mode() string
	SetEffort(effort string) // "" is the model's default
	Effort() string
	// Attach queues files for the next Send.
	Attach(a ...client.Attachment)
}

// ThreadAdminBackend renames, pins and deletes conversations.
type ThreadAdminBackend interface {
	RenameSession(ctx context.Context, id, title string) error
	PinSession(ctx context.Context, id string, pinned bool) error
	DeleteSession(ctx context.Context, id string) error
	// CurrentThread is the id of the current conversation.
	CurrentThread() string
}

func (b *ClientBackend) extras() (client.Extras, bool) { x, ok := b.C.(client.Extras); return x, ok }

// Compact implements HistoryBackend.
func (b *ClientBackend) Compact(ctx context.Context) (*client.CompactResult, error) {
	x, ok := b.extras()
	if !ok {
		return nil, client.ErrNotSupported
	}
	return x.Compact(ctx, b.ThreadID)
}

// Undo implements HistoryBackend.
func (b *ClientBackend) Undo(ctx context.Context) (*client.UndoResult, error) {
	x, ok := b.extras()
	if !ok {
		return nil, client.ErrNotSupported
	}
	return x.Undo(ctx, b.ThreadID)
}

// Redo implements HistoryBackend.
func (b *ClientBackend) Redo(ctx context.Context) (*client.UndoResult, error) {
	x, ok := b.extras()
	if !ok {
		return nil, client.ErrNotSupported
	}
	return x.Redo(ctx, b.ThreadID)
}

// Fork implements HistoryBackend.
func (b *ClientBackend) Fork(ctx context.Context, messageID string) (*client.Thread, error) {
	x, ok := b.extras()
	if !ok {
		return nil, client.ErrNotSupported
	}
	th, err := x.Fork(ctx, b.ThreadID, messageID)
	if err != nil {
		return nil, err
	}
	b.ThreadID = th.ID
	return th, nil
}

// Commands implements CommandBackend.
func (b *ClientBackend) Commands(ctx context.Context) ([]client.Command, error) {
	x, ok := b.extras()
	if !ok {
		return nil, client.ErrNotSupported
	}
	return x.Commands(ctx)
}

// ExpandCommand implements CommandBackend.
func (b *ClientBackend) ExpandCommand(ctx context.Context, name, args string) (string, error) {
	x, ok := b.extras()
	if !ok {
		return "", client.ErrNotSupported
	}
	return x.ExpandCommand(ctx, name, args)
}

// SetMode implements RunOptionsBackend.
func (b *ClientBackend) SetMode(mode string) { b.RunMode = mode }

// Mode implements RunOptionsBackend.
func (b *ClientBackend) Mode() string {
	if b.RunMode == "" {
		return client.ModeBuild
	}
	return b.RunMode
}

// SetEffort implements RunOptionsBackend.
func (b *ClientBackend) SetEffort(e string) { b.ReasoningEffort = e }

// Effort implements RunOptionsBackend.
func (b *ClientBackend) Effort() string { return b.ReasoningEffort }

// Attach implements RunOptionsBackend.
func (b *ClientBackend) Attach(a ...client.Attachment) { b.attach = append(b.attach, a...) }

// RenameSession implements ThreadAdminBackend.
func (b *ClientBackend) RenameSession(ctx context.Context, id, title string) error {
	_, err := b.C.RenameThread(ctx, id, client.ThreadPatch{Title: &title})
	return err
}

// PinSession implements ThreadAdminBackend.
func (b *ClientBackend) PinSession(ctx context.Context, id string, pinned bool) error {
	_, err := b.C.RenameThread(ctx, id, client.ThreadPatch{Pinned: &pinned})
	return err
}

// DeleteSession implements ThreadAdminBackend.
func (b *ClientBackend) DeleteSession(ctx context.Context, id string) error {
	return b.C.DeleteThread(ctx, id)
}

// CurrentThread implements ThreadAdminBackend.
func (b *ClientBackend) CurrentThread() string { return b.ThreadID }
