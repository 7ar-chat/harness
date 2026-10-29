package client

import "context"

var _ Extras = (*httpClient)(nil)

func (c *httpClient) Compact(context.Context, string) (*CompactResult, error) {
	return nil, ErrNotSupported
}
func (c *httpClient) Undo(context.Context, string) (*UndoResult, error) { return nil, ErrNotSupported }
func (c *httpClient) Redo(context.Context, string) (*UndoResult, error) { return nil, ErrNotSupported }
func (c *httpClient) Fork(context.Context, string, string) (*Thread, error) {
	return nil, ErrNotSupported
}
