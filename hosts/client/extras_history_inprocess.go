package client

import (
	"context"
	"errors"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/core/store"
)

// historyErr turns the errors of the history operations into the API errors.
func historyErr(err error) error {
	switch {
	case errors.Is(err, store.ErrRunActive):
		return errorf(ErrConflict, "The thread has a run in progress.")
	case errors.Is(err, harness.ErrNothingToUndo):
		return errorf(ErrConflict, "Nothing to undo.")
	case errors.Is(err, harness.ErrNothingToRedo):
		return errorf(ErrConflict, "Nothing to redo.")
	case errors.Is(err, harness.ErrHistoryUnsupported):
		return ErrNotSupported
	}
	return mapErr(err)
}

func (c *inProcess) Compact(ctx context.Context, threadID string) (*CompactResult, error) {
	res, err := c.h.Compact(ctx, c.user, threadID, c.def)
	if err != nil {
		return nil, historyErr(err)
	}
	return &CompactResult{Compacted: res.Compacted, TokensBefore: res.TokensBefore, TokensAfter: res.TokensAfter}, nil
}

func (c *inProcess) Undo(ctx context.Context, threadID string) (*UndoResult, error) {
	step, err := c.h.Undo(ctx, c.user, threadID)
	if err != nil {
		return nil, historyErr(err)
	}
	return toUndoResult(step), nil
}

func (c *inProcess) Redo(ctx context.Context, threadID string) (*UndoResult, error) {
	step, err := c.h.Redo(ctx, c.user, threadID)
	if err != nil {
		return nil, historyErr(err)
	}
	return toUndoResult(step), nil
}

func toUndoResult(s *harness.HistoryStep) *UndoResult {
	return &UndoResult{Prompt: s.Prompt, Files: s.Files, CanUndo: s.CanUndo, CanRedo: s.CanRedo}
}

func (c *inProcess) Fork(ctx context.Context, threadID, messageID string) (*Thread, error) {
	th, err := c.h.Fork(ctx, c.user, threadID, messageID)
	if err != nil {
		return nil, historyErr(err)
	}
	d := toThread(th)
	return &d, nil
}
