// Package tools defines tool providers assembled per run.
package tools

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/cloudwego/eino/components/tool"
)

// RunContext identifies the run a tool set is built for.
type RunContext struct {
	ThreadID string
	RunID    string
	Login    string
}

// Provider contributes tools to a run.
type Provider interface {
	Name() string
	// Tools returns the provider's tools for this run and a cleanup func called at run end.
	Tools(ctx context.Context, rc RunContext) ([]tool.BaseTool, func(), error)
}

// ApprovalNamer is a tool whose exposed name may not show what it does (MCP:
// sanitized and truncated to 64 characters): ApprovalNames are the original
// names its riskiness (interactive approvals) is decided from.
type ApprovalNamer interface {
	ApprovalNames() []string
}

// Registry assembles all providers; a failing provider is logged and skipped.
type Registry struct {
	providers []Provider
}

// NewRegistry returns a registry of providers (nil entries are ignored).
func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{}
	for _, p := range providers {
		if p != nil {
			r.providers = append(r.providers, p)
		}
	}
	return r
}

// Name implements Provider.
func (r *Registry) Name() string { return "registry" }

// Tools implements Provider: merges every provider's tools, dropping duplicate names.
func (r *Registry) Tools(ctx context.Context, rc RunContext) ([]tool.BaseTool, func(), error) {
	var all []tool.BaseTool
	var cleanups []func()
	seen := map[string]bool{}
	for _, p := range r.providers {
		ts, cleanup, err := p.Tools(ctx, rc)
		if err != nil {
			slog.Warn("tool provider failed, skipping", "provider", p.Name(), "run_id", rc.RunID, "error", err)
			continue
		}
		if cleanup != nil {
			cleanups = append(cleanups, cleanup)
		}
		for _, t := range ts {
			info, err := t.Info(ctx)
			if err != nil || info == nil {
				continue
			}
			if seen[info.Name] {
				slog.Warn("duplicate tool name, skipping", "provider", p.Name(), "tool", info.Name)
				continue
			}
			seen[info.Name] = true
			all = append(all, t)
		}
	}
	return all, func() {
		for _, c := range cleanups {
			c()
		}
	}, nil
}

// Emitter receives live tool output.
type Emitter interface {
	ToolOutput(toolCallID, stream, text string)
}

type emitterKey struct{}

type callInfo struct {
	toolCallID string
	emitter    Emitter
}

// WithCall attaches the current tool call id and emitter to ctx.
func WithCall(ctx context.Context, toolCallID string, e Emitter) context.Context {
	return context.WithValue(ctx, emitterKey{}, callInfo{toolCallID: toolCallID, emitter: e})
}

// CallFromContext returns the tool call id and emitter (nil emitter when absent).
func CallFromContext(ctx context.Context) (string, Emitter) {
	ci, _ := ctx.Value(emitterKey{}).(callInfo)
	return ci.toolCallID, ci.emitter
}

// UserError is a tool failure whose message is meant for the model.
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

// Errorf builds a UserError.
func Errorf(format string, args ...any) error { return &UserError{Msg: fmt.Sprintf(format, args...)} }

// StopGuard is an optional interface of a tool. When the model answers without
// tool calls and the run still has steps left, the agent asks every StopGuard
// tool of the run (in tool-name order); non-empty reminders are given to the
// model as one unpersisted user message and the run continues (at most 2 per run).
type StopGuard interface {
	StopReminder(ctx context.Context) string
}

// OutputSpiller writes a tool output too large for the model's context to a
// file of the run's workspace and returns its path (harness v3 §2).
type OutputSpiller interface {
	SpillOutput(ctx context.Context, rc RunContext, toolCallID, content string) (path string, err error)
}
