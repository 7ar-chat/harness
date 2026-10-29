package client

import "context"

func (c *inProcess) Commands(context.Context) ([]Command, error) { return nil, ErrNotSupported }
func (c *inProcess) ExpandCommand(context.Context, string, string) (string, error) {
	return "", ErrNotSupported
}
