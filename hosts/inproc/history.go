package inproc

import (
	"context"
	"errors"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/hosts/client"
)

// historyErr turns the errors of the history operations into the API errors.
func historyErr(err error) error {
	switch {
	case errors.Is(err, store.ErrRunActive):
		return client.Errorf(client.ErrConflict, "The thread has a run in progress.")
	case errors.Is(err, harness.ErrNothingToUndo):
		return client.Errorf(client.ErrConflict, "Nothing to undo.")
	case errors.Is(err, harness.ErrNothingToRedo):
		return client.Errorf(client.ErrConflict, "Nothing to redo.")
	case errors.Is(err, harness.ErrHistoryUnsupported):
		return client.ErrNotSupported
	}
	return mapErr(err)
}

func (c *impl) Compact(ctx context.Context, threadID string) (*client.CompactResult, error) {
	res, err := c.h.Compact(ctx, c.user, threadID, c.def)
	if err != nil {
		return nil, historyErr(err)
	}
	return &client.CompactResult{Compacted: res.Compacted, TokensBefore: res.TokensBefore, TokensAfter: res.TokensAfter}, nil
}

func (c *impl) Undo(ctx context.Context, threadID string) (*client.UndoResult, error) {
	step, err := c.h.Undo(ctx, c.user, threadID)
	if err != nil {
		return nil, historyErr(err)
	}
	return toUndoResult(step), nil
}

func (c *impl) Redo(ctx context.Context, threadID string) (*client.UndoResult, error) {
	step, err := c.h.Redo(ctx, c.user, threadID)
	if err != nil {
		return nil, historyErr(err)
	}
	return toUndoResult(step), nil
}

func toUndoResult(s *harness.HistoryStep) *client.UndoResult {
	return &client.UndoResult{Prompt: s.Prompt, Files: s.Files, CanUndo: s.CanUndo, CanRedo: s.CanRedo}
}

func (c *impl) Fork(ctx context.Context, threadID, messageID string) (*client.Thread, error) {
	th, err := c.h.Fork(ctx, c.user, threadID, messageID)
	if err != nil {
		return nil, historyErr(err)
	}
	d := toThread(th)
	return &d, nil
}
