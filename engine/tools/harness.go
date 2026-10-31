package tools

import (
	"context"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
)

// QuestionOption is an answer choice of an Interrupt.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Interrupt is returned (as error) by a tool that needs the user before the
// agent can continue (ask_user): the run ends with an AG-UI interrupt outcome
// and the tool result is supplied by the user when the run is resumed.
type Interrupt struct {
	Reason        string
	Question      string
	Options       []QuestionOption
	AllowFreeText bool
	// ID and ToolCallID are set by the agent.
	ID         string
	ToolCallID string
	// Approval is set for a risky tool call paused before it runs (reason
	// tool_approval, interactive mode); ID is then the approval id.
	Approval *store.Approval
}

func (i *Interrupt) Error() string { return "interrupt: " + i.Reason }

// ResultEmitter is implemented by the agent's emitter: events queued with
// EmitAfterResult are written right after the tool's TOOL_CALL_RESULT.
type ResultEmitter interface {
	EmitAfterResult(ev agui.Event)
}

// AfterResult queues an AG-UI event to follow the current tool call's result
// (dropped when the call has no agent emitter, e.g. in isolation).
func AfterResult(ctx context.Context, ev agui.Event) {
	if _, e := CallFromContext(ctx); e != nil {
		if re, ok := e.(ResultEmitter); ok {
			re.EmitAfterResult(ev)
		}
	}
}

// LiveEmitter is implemented by the agent's emitter: events emitted with Live
// are written while the tool runs (in call order when calls run in parallel),
// and sub-agent usage is added to the run's usage totals.
type LiveEmitter interface {
	EmitLive(ev agui.Event)
	AddSubagentUsage(model string, in, out, cached int)
}

// Live emits an AG-UI event while the current tool call runs (dropped when the
// call has no agent emitter, e.g. in isolation).
func Live(ctx context.Context, ev agui.Event) {
	if _, e := CallFromContext(ctx); e != nil {
		if le, ok := e.(LiveEmitter); ok {
			le.EmitLive(ev)
		}
	}
}

// SubagentUsage adds the token usage of a sub-agent's model calls to the run's
// totals (reported under its own model name; no-op without an agent emitter).
func SubagentUsage(ctx context.Context, model string, in, out, cached int) {
	if _, e := CallFromContext(ctx); e != nil {
		if le, ok := e.(LiveEmitter); ok {
			le.AddSubagentUsage(model, in, out, cached)
		}
	}
}

// UntrustedSource is implemented by the agent's emitter: it reports whether
// the model's context holds untrusted content (tool output, attachments, a
// user skill, a compaction summary) at the time of the current tool call.
type UntrustedSource interface {
	Untrusted() bool
}

// Untrusted reports whether the current call follows untrusted content in the
// model's context (false without an agent emitter).
func Untrusted(ctx context.Context) bool {
	if _, e := CallFromContext(ctx); e != nil {
		if u, ok := e.(UntrustedSource); ok {
			return u.Untrusted()
		}
	}
	return false
}
