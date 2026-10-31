package client

import (
	"context"
	"net/url"
)

// missingRoute reports a 404 outside the error envelope: the server does not know the route.
func missingRoute(err error) bool {
	e := AsError(err)
	return e != nil && e.Status == 404 && e.Code == "http_404"
}

// historyCall posts to a thread's history route; a server without the route (a 404 outside the error
// envelope) is an older server: ErrNotSupported.
func (c *httpClient) historyCall(ctx context.Context, threadID, op string, body, out any) error {
	err := c.call(ctx, "POST", "/threads/"+url.PathEscape(threadID)+"/"+op, nil, body, out)
	if missingRoute(err) {
		return ErrNotSupported
	}
	return err
}

func (c *httpClient) Compact(ctx context.Context, threadID string) (*CompactResult, error) {
	var r CompactResult
	if err := c.historyCall(ctx, threadID, "compact", nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *httpClient) Undo(ctx context.Context, threadID string) (*UndoResult, error) {
	var r UndoResult
	if err := c.historyCall(ctx, threadID, "undo", nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *httpClient) Redo(ctx context.Context, threadID string) (*UndoResult, error) {
	var r UndoResult
	if err := c.historyCall(ctx, threadID, "redo", nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *httpClient) Fork(ctx context.Context, threadID, messageID string) (*Thread, error) {
	var t Thread
	if err := c.historyCall(ctx, threadID, "fork", map[string]string{"messageId": messageID}, &t); err != nil {
		return nil, err
	}
	return &t, nil
}
