package client

import "context"

func (c *httpClient) Commands(context.Context) ([]Command, error) { return nil, ErrNotSupported }
func (c *httpClient) ExpandCommand(context.Context, string, string) (string, error) {
	return "", ErrNotSupported
}
