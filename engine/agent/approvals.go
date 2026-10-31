package agent

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"log/slog"
	"strings"
	"time"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/compaction"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/approvals"
	"github.com/7ar-chat/harness/engine/tools"
)

const askUserTool = "ask_user"

var errPausePanicked = errors.New("the approval hook crashed")

// callTool runs one tool call of a step. In interactive mode a risky call
// pauses the run before it executes; once the step has an interrupt, later
// calls that need the user (risky calls, ask_user after an approval) are
// deferred without a result: the resumed run takes them up one at a time,
// in order. Other calls of the step still run.
func (st *runState) callTool(ctx context.Context, tc ai.ToolCall, toolMap map[string]ai.BaseTool) error {
	if refusal := st.denial(ctx, tc, toolMap); refusal != "" {
		st.g.observed(tc, refusal, true)
		return st.refuseTool(ctx, tc, refusal)
	}
	if intr := st.h.interrupt; intr != nil && intr.Approval != nil && st.hookFor(ctx, tc, toolMap, risky) != nil {
		// A call waits for the user here: later risky calls stay behind it whatever
		// the live mode (a switch to automatic must not run them before it).
		return nil
	}
	gate := st.gate(ctx, tc, toolMap) // approvals.Gate: scope "run" kinds, work areas, risky MCP tools (§7)
	if intr := st.h.interrupt; intr != nil && (gate != nil || (tc.Function.Name == askUserTool && intr.Approval != nil)) {
		return nil
	}
	if gate != nil {
		if refusal := st.g.peekLoop(tc); refusal != "" { // never ask to approve a call the loop guard refuses
			st.g.observed(tc, refusal, true)
			return st.refuseTool(ctx, tc, refusal)
		}
		return st.pause(ctx, tc, gate)
	}
	return st.runTool(ctx, tc, toolMap)
}

// denial asks the hooks that can refuse calls (ToolDenier) for a refusal text
// ("" runs the call as usual; a panicking hook refuses: fail closed).
func (st *runState) denial(ctx context.Context, tc ai.ToolCall, toolMap map[string]ai.BaseTool) (text string) {
	if len(st.a.Harness.BeforeTool) == 0 || toolMap[tc.Function.Name] == nil {
		return ""
	}
	args := strings.TrimSpace(tc.Function.Arguments)
	if args == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		return ""
	}
	for _, h := range st.a.Harness.BeforeTool {
		d, ok := h.(ToolDenier)
		if !ok || text != "" {
			continue
		}
		func() {
			defer func() {
				if p := recover(); p != nil {
					slog.Error("agent: before-tool hook panicked", "run_id", st.in.Run.ID, "panic", p)
					text = "Error: the permission check crashed, so the action was not run."
				}
			}()
			text = d.Denial(ctx, st.in.Run, tc.Function.Name, tc.Function.Arguments)
		}()
	}
	return text
}

// gate returns the hook that requires approval for a call (nil: run it). Unknown
// tools and invalid arguments are not gated: invoking them has no effect.
func (st *runState) gate(ctx context.Context, tc ai.ToolCall, toolMap map[string]ai.BaseTool) BeforeToolHook {
	return st.hookFor(ctx, tc, toolMap, needsApproval)
}

// hookFor returns the first hook for which check holds for one of the call's
// names (nil: none, an unknown tool or invalid arguments).
func (st *runState) hookFor(ctx context.Context, tc ai.ToolCall, toolMap map[string]ai.BaseTool,
	check func(ctx context.Context, run *store.Run, h BeforeToolHook, toolName, arguments string) bool) BeforeToolHook {
	if len(st.a.Harness.BeforeTool) == 0 || toolMap[tc.Function.Name] == nil {
		return nil
	}
	args := strings.TrimSpace(tc.Function.Arguments)
	if args == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		return nil
	}
	names := []string{tc.Function.Name}
	if n, ok := toolMap[tc.Function.Name].(tools.ApprovalNamer); ok {
		// MCP tools: also their original names (the exposed one may be truncated).
		names = append(names, n.ApprovalNames()...)
	}
	for _, h := range st.a.Harness.BeforeTool {
		for _, name := range names {
			if check(ctx, st.in.Run, h, name, tc.Function.Arguments) {
				return h
			}
		}
	}
	return nil
}

// needsApproval asks a hook; a panicking hook gates the call (fail closed).
func needsApproval(ctx context.Context, run *store.Run, h BeforeToolHook, toolName, arguments string) (gated bool) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("agent: before-tool hook panicked", "run_id", run.ID, "panic", p)
			gated = true
		}
	}()
	return h.NeedsApproval(ctx, run, toolName, arguments)
}

// risky asks a hook whether a call would need approval in interactive mode
// (RiskJudge; NeedsApproval for other hooks); a panicking hook says yes.
func risky(ctx context.Context, run *store.Run, h BeforeToolHook, toolName, arguments string) (yes bool) {
	j, ok := h.(RiskJudge)
	if !ok {
		return needsApproval(ctx, run, h, toolName, arguments)
	}
	defer func() {
		if p := recover(); p != nil {
			slog.Error("agent: before-tool hook panicked", "run_id", run.ID, "panic", p)
			yes = true
		}
	}()
	return j.Risky(ctx, run, toolName, arguments)
}

// pause records the call's pending approval and ends the run after this step.
// When the approval cannot be recorded the call is refused (never run unapproved).
func (st *runState) pause(ctx context.Context, tc ai.ToolCall, h BeforeToolHook) error {
	intr, err := func() (intr *tools.Interrupt, err error) {
		defer func() {
			if p := recover(); p != nil {
				slog.Error("agent: approval pause panicked", "run_id", st.in.Run.ID, "panic", p)
				intr, err = nil, errPausePanicked
			}
		}()
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return h.Pause(pctx, st.in.Run, tc)
	}()
	if err != nil || intr == nil || intr.Approval == nil {
		slog.Error("agent: recording the approval failed; refusing the call", "run_id", st.in.Run.ID, "tool", tc.Function.Name, "error", err)
		return st.refuseTool(ctx, tc, approvals.PauseFailedResult)
	}
	intr.ToolCallID = tc.ID
	if intr.ID == "" {
		intr.ID = intr.Approval.ID
	}
	st.h.interrupt = intr
	return st.emit(ctx, agui.CustomEvent(agui.CustomApproval, map[string]any{"phase": "required", "approval": intr.Approval.Payload()}))
}

// refuseTool gives a call an error result without running it.
func (st *runState) refuseTool(ctx context.Context, tc ai.ToolCall, content string) error {
	id := uuid.NewString()
	if err := st.emit(ctx, agui.New(agui.ToolCallResult, map[string]any{
		"messageId": id, "toolCallId": tc.ID, "content": content, "role": "tool", "metadata": map[string]any{"isError": true},
	})); err != nil {
		return err
	}
	row := &store.Message{ID: id, Role: store.RoleTool, Content: content, ToolCallID: tc.ID, Error: truncate(content, 2000)}
	st.persist(ctx, row)
	hm := ai.ToolMessage(content, tc.ID, ai.WithToolName(tc.Function.Name))
	compaction.SetPosition(hm, row.Position)
	st.addToolResult(tc.ID, hm)
	return nil
}

// addToolResult appends a tool result to the history, or fills the slot of a
// call finished by a resumed run (its placeholder result follows the call).
func (st *runState) addToolResult(toolCallID string, hm *ai.Message) {
	if i, ok := st.h.slots[toolCallID]; ok && i < len(st.history) {
		st.history[i] = hm
		delete(st.h.slots, toolCallID)
		return
	}
	st.history = append(st.history, hm)
}

// emitApprovalResolved loads the approval this run decides (or cancelled) and announces it.
func (st *runState) emitApprovalResolved(ctx context.Context) error {
	src := st.a.Harness.Approvals
	if src == nil {
		return nil
	}
	a, err := src.ResumedApproval(ctx, st.in.Run.ID)
	if err != nil {
		slog.Warn("agent: loading the resumed approval failed", "run_id", st.in.Run.ID, "error", err)
		return nil
	}
	if a == nil {
		return nil
	}
	st.h.approval = a
	return st.emit(ctx, agui.CustomEvent(agui.CustomApproval, map[string]any{"phase": "resolved", "approval": a.Payload()}))
}

// resumeStep finishes the step a resumed run continues (an answered question
// or a decided approval, without a new user message): the approved call runs
// with its exact arguments (refused when they changed), then the calls that
// were deferred, in order (a risky one pauses the run again). paused reports
// that the run ends on a new interrupt before any model call.
func (st *runState) resumeStep(ctx context.Context, msgs []*store.Message, toolMap map[string]ai.BaseTool) (paused bool, err error) {
	if st.h.ownMessage {
		return false, nil // a new message ended the interrupted step
	}
	a := st.h.approval
	anchor := ""
	switch {
	case a != nil && a.Status == store.ApprovalResolved:
		anchor = a.ToolCallID
	case len(st.h.resumed) > 0:
		anchor = st.h.resumed[0].ToolCallID
	default:
		return false, nil
	}
	calls := unansweredCalls(msgs, anchor)
	if len(calls) == 0 {
		return false, nil
	}
	st.h.slots = map[string]int{}
	defer func() { st.h.slots = nil }()
	for i, m := range st.history {
		if m.Role == ai.RoleTool {
			for _, tc := range calls {
				if tc.ID == m.ToolCallID {
					st.h.slots[tc.ID] = i
				}
			}
		}
	}
	for _, tc := range calls {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if _, ok := st.h.slots[tc.ID]; !ok {
			continue // not in the model's view any more (compacted)
		}
		if a != nil && a.Status == store.ApprovalResolved && a.Decision == store.DecisionApprove && tc.ID == a.ToolCallID {
			if !approvals.MatchesCall(a, tc) {
				if err := st.refuseTool(ctx, tc, approvals.MismatchResult); err != nil {
					return false, err
				}
				continue
			}
			if err := st.runTool(ctx, tc, toolMap); err != nil {
				return false, err
			}
			continue
		}
		if err := st.callTool(ctx, tc, toolMap); err != nil {
			return false, err
		}
	}
	return st.h.interrupt != nil, nil
}

// unansweredCalls are the calls of the assistant message holding anchor that have no result yet.
func unansweredCalls(msgs []*store.Message, anchor string) []ai.ToolCall {
	answered := map[string]bool{}
	var holder *store.Message
	for _, m := range msgs {
		switch m.Role {
		case store.RoleTool:
			answered[m.ToolCallID] = true
		case store.RoleAssistant:
			for _, c := range m.ToolCalls {
				if c.ID == anchor {
					holder = m
				}
			}
		}
	}
	if holder == nil {
		return nil
	}
	var out []ai.ToolCall
	for _, c := range holder.ToolCalls {
		if !answered[c.ID] {
			out = append(out, ai.ToolCall{ID: c.ID, Type: "function", Function: ai.FunctionCall{Name: c.Function.Name, Arguments: c.Function.Arguments}})
		}
	}
	return out
}
