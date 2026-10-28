package tools

import (
	"context"

	"github.com/cloudwego/eino/components/tool"
)

// Filter is a Provider that offers only the tools of Inner that Allow accepts
// (called per run, in Inner's order, so the tool list stays byte-stable).
type Filter struct {
	Inner Provider
	Allow func(ctx context.Context, rc RunContext, name string) bool
}

// Name implements Provider.
func (f Filter) Name() string { return f.Inner.Name() }

// Tools implements Provider.
func (f Filter) Tools(ctx context.Context, rc RunContext) ([]tool.BaseTool, func(), error) {
	ts, cleanup, err := f.Inner.Tools(ctx, rc)
	if err != nil {
		return nil, cleanup, err
	}
	out := ts[:0:0]
	for _, t := range ts {
		info, err := t.Info(ctx)
		if err != nil || info == nil || !f.Allow(ctx, rc, info.Name) {
			continue
		}
		out = append(out, t)
	}
	return out, cleanup, nil
}
