package client

import (
	"context"
	"errors"
	"net/url"
)

func (c *httpClient) Commands(ctx context.Context) ([]Command, error) {
	var out []Command
	if err := c.call(ctx, "GET", "/commands", nil, nil, &out); err != nil {
		if errors.Is(err, ErrNotFound) || missingRoute(err) {
			return nil, ErrNotSupported // a server without the route
		}
		return nil, err
	}
	return out, nil
}

func (c *httpClient) ExpandCommand(ctx context.Context, name, args string) (string, error) {
	var out struct {
		Prompt string `json:"prompt"`
	}
	err := c.call(ctx, "POST", "/commands/"+url.PathEscape(name)+"/expand", nil, map[string]string{"args": args}, &out)
	return out.Prompt, err
}
