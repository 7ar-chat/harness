package client

import "context"

var _ Extras = (*inProcess)(nil)

func (c *inProcess) Compact(context.Context, string) (*CompactResult, error) {
	return nil, ErrNotSupported
}
func (c *inProcess) Undo(context.Context, string) (*UndoResult, error) { return nil, ErrNotSupported }
func (c *inProcess) Redo(context.Context, string) (*UndoResult, error) { return nil, ErrNotSupported }
func (c *inProcess) Fork(context.Context, string, string) (*Thread, error) {
	return nil, ErrNotSupported
}
