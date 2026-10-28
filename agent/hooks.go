package agent

import (
	"context"
	"log/slog"
	"strings"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/agui"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/tools"
)

// ToolHook observes every tool result (not cancelled calls, not the ask_user
// interrupt). It runs after the call's TOOL_CALL_RESULT; its events go through
// tools.AfterResult(ctx, …) — the queue the tools themselves use — so they are
// written right after the result and before STEP_FINISHED. The metadata it
// returns is stored on the tool message (e.g. metadata.artifacts).
type ToolHook interface {
	AfterTool(ctx context.Context, run *store.Run, toolCallID, toolName string) map[string]any
}

// BeforeToolHook runs before a tool call executes (interactive approvals).
// NeedsApproval must not have side effects (it also decides which later calls
// of a step wait for an earlier decision); Pause records the pending approval
// and returns the interrupt that ends the run before the call runs.
type BeforeToolHook interface {
	// ctx is the run's: Stop cancels a decision that needs the sandbox (round 3, N5).
	NeedsApproval(ctx context.Context, run *store.Run, toolName, arguments string) bool
	Pause(ctx context.Context, run *store.Run, tc schema.ToolCall) (*tools.Interrupt, error)
}

// RiskJudge is implemented by BeforeToolHooks that can tell whether a call
// would need approval in interactive mode, whatever the run's current mode is.
// Once a step has a pending approval, later calls it judges risky wait behind
// that approval, so a switch to automatic mid-step never lets them run before
// the paused call. Like NeedsApproval it must not have side effects.
type RiskJudge interface {
	Risky(ctx context.Context, run *store.Run, toolName, arguments string) bool
}

// ApprovalSource returns the approval a run was created to decide (or that
// its message cancelled); nil when none.
type ApprovalSource interface {
	ResumedApproval(ctx context.Context, runID string) (*store.Approval, error)
}

// RunStartHook runs at the start of every run except compactions, before its
// first model call; it must not fail the run (errors are its own to log).
type RunStartHook interface {
	BeforeRun(ctx context.Context, run *store.Run)
}

// beforeRun runs the run-start hooks (a panic is logged and ignored).
func (st *runState) beforeRun(ctx context.Context) {
	for _, h := range st.a.Harness.RunStart {
		func() {
			defer func() {
				if p := recover(); p != nil {
					slog.Error("agent: run start hook panicked", "run_id", st.in.Run.ID, "panic", p)
				}
			}()
			h.BeforeRun(ctx, st.in.Run)
		}()
	}
}

// AnswerChecker reviews the final answer of a run (not cancelled, not failed,
// not waiting for the user) that still has steps left. A non-empty note is
// given to the model as a user-side message for one corrective step (at most
// once per run; the note is not persisted). emit writes events of existing types.
type AnswerChecker interface {
	CheckAnswer(ctx context.Context, run *store.Run, answer string, emit func(agui.Event) error) string
}

// checkAnswer runs the answer checker (a panic counts as no note).
func (st *runState) checkAnswer(ctx context.Context) (note string) {
	c := st.a.Harness.Answers
	if c == nil {
		return ""
	}
	defer func() {
		if p := recover(); p != nil {
			slog.Error("agent: answer check panicked", "run_id", st.in.Run.ID, "panic", p)
			note = ""
		}
	}()
	return c.CheckAnswer(ctx, st.in.Run, st.h.answer, func(ev agui.Event) error { return st.emit(ctx, ev) })
}

// ImageAttacher adds attached images as image parts for vision models.
type ImageAttacher interface {
	AttachImages(ctx context.Context, run *store.Run, history []*schema.Message) []*schema.Message
}

// afterTool runs the tool hooks with the call's emitter in ctx and merges their metadata.
func (st *runState) afterTool(ctx context.Context, tc schema.ToolCall, em *toolEmitter) map[string]any {
	if ctx.Err() != nil {
		return nil // cancelled: the end-of-run hook catches up
	}
	hctx := tools.WithCall(ctx, tc.ID, em)
	var metadata map[string]any
	for _, h := range st.a.Harness.ToolHooks {
		for k, v := range safeToolHook(st.in.Run.ID, func() map[string]any { return h.AfterTool(hctx, st.in.Run, tc.ID, tc.Function.Name) }) {
			if metadata == nil {
				metadata = map[string]any{}
			}
			metadata[k] = v
		}
	}
	return metadata
}

func safeToolHook(runID string, fn func() map[string]any) (out map[string]any) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("agent: tool hook panicked", "run_id", runID, "panic", p)
			out = nil
		}
	}()
	return fn()
}

// userMessage is the model view of a user message: its attachments are listed
// after the text ("[Attached files: …]") and kept in Extra for ImageAttacher.
func userMessage(m *store.Message, content string) *schema.Message {
	atts := store.MessageAttachments(m)
	if len(atts) == 0 {
		return schema.UserMessage(content)
	}
	paths := make([]string, 0, len(atts))
	for _, a := range atts {
		paths = append(paths, a.Path)
	}
	line := "[Attached files: " + strings.Join(paths, ", ") + "]"
	if text := strings.TrimRight(content, "\n"); strings.TrimSpace(text) != "" {
		line = text + "\n\n" + line
	}
	msg := schema.UserMessage(line)
	msg.Extra = map[string]any{store.ExtraAttachments: atts}
	return msg
}

func (a *Agent) attachImages(ctx context.Context, run *store.Run, history []*schema.Message) (out []*schema.Message) {
	out = history
	if a.Harness.Images == nil {
		return out
	}
	defer func() {
		if p := recover(); p != nil {
			slog.Error("agent: attaching images panicked", "run_id", run.ID, "panic", p)
		}
	}()
	return a.Harness.Images.AttachImages(ctx, run, history)
}
