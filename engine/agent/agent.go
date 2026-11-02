// Package agent is the agent loop: an own ReAct loop on eino
// ToolCallingChatModel that streams AG-UI events and checkpoints messages.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/7ar-chat/harness/ai"
	aiagent "github.com/7ar-chat/harness/ai/agent"
	"github.com/7ar-chat/harness/ai/compaction"
	"github.com/7ar-chat/harness/ai/llm"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/eventlog"
	"github.com/7ar-chat/harness/engine/queue"
	"github.com/7ar-chat/harness/engine/tools"
)

// SteerPrefix prefixes steer messages in the model history.
const SteerPrefix = "[The user added while you were working]: "

// StepLimitNudge is appended on the last allowed step.
const StepLimitNudge = "You have reached the step limit; answer with what you have."

const (
	oldToolOutputChars = 2000
	interruptedResult  = "The tool call was interrupted before it returned a result."
	cancelledResult    = "The tool call was cancelled by the user."
)

// Error is a run failure with an AG-UI RUN_ERROR code.
type Error struct {
	Code    string
	Message string
	// Detail is logged, never sent to clients.
	Detail string
	cause  error
}

// Unwrap returns the underlying model error, if any.
func (e *Error) Unwrap() error { return e.cause }

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Code + ": " + e.Message + " (" + e.Detail + ")"
	}
	return e.Code + ": " + e.Message
}

// Messages sent to clients (RUN_ERROR, run.error): never raw provider errors or hostnames.
const (
	MsgModelError = "The AI model could not answer. Please try again in a moment."
	MsgInternal   = "An internal error occurred."
)

// SteerSource drains steer messages of a run. FinalizeSteers atomically stops
// accepting steers (PushSteer then fails) and drains; ReopenSteers accepts again.
type SteerSource interface {
	DrainSteers(ctx context.Context, runID string) ([]queue.Steer, error)
	FinalizeSteers(ctx context.Context, runID string) ([]queue.Steer, error)
	ReopenSteers(ctx context.Context, runID string) error
}

// MessageStore is the subset of store.Store used by the agent.
type MessageStore interface {
	ListMessages(ctx context.Context, threadID string) ([]*store.Message, error)
	AppendMessages(ctx context.Context, msgs ...*store.Message) error
}

// Agent runs the loop.
type Agent struct {
	Models             llm.Factory
	Tools              tools.Provider
	Store              MessageStore
	Steers             SteerSource
	MaxSteps           int
	MaxToolOutputChars int
	// RetryBackoff is the first model retry delay (doubles each retry, ±50 % jitter).
	RetryBackoff time.Duration
	// ModelTimeout bounds the wait for the first chunk of a model call (LLM_TIMEOUT_SECONDS; 0: 300 s).
	ModelTimeout time.Duration
	// StreamIdleTimeout bounds the gap between two chunks (LLM_STREAM_IDLE_SECONDS; 0: 90 s).
	StreamIdleTimeout time.Duration
	// CallCeiling bounds one model call whatever its stream sends
	// (LLM_CALL_MAX_SECONDS; 0: 20 min; ×2 at high, ×3 at xhigh effort). Round 3, N3.
	CallCeiling time.Duration
	// StepRetries is how often a failed model call is re-issued as a whole step
	// (LLM_STEP_RETRIES; 0: 3; < 0: never).
	StepRetries int
	// Sleep waits between retries (nil: a timer; tests record the delays).
	Sleep func(ctx context.Context, d time.Duration) error
	// SpillChars: a tool output longer than this is written in full to
	// /workspace/.work/tool-output/<toolCallId>.txt and the model gets its head
	// and tail (AGENT_TOOL_OUTPUT_SPILL_CHARS; 0: 20000).
	SpillChars int
	Now        func() time.Time
	// Harness holds the harness v2 options (prompt sections, tool hooks, images,
	// compaction, reasoning).
	Harness Harness
}

// Input is one run to execute.
type Input struct {
	Run       *store.Run
	FirstName string
	// Sandbox is set by the agent before the prompt sections run: the run has sandbox tools.
	Sandbox bool
	// Variant is the run's prompt variant (prompts.Default, prompts.Legacy…),
	// set by the agent before the prompt sections run.
	Variant string
}

// Result is a successful run outcome.
type Result struct {
	Usage []agui.Usage
	// Interrupt is set when the run ended waiting for the user (ask_user).
	Interrupt *tools.Interrupt
}

type runState struct {
	a       *Agent
	in      Input
	w       eventlog.Writer
	history []*ai.Message
	usage   agui.Usage
	carry   []queue.Steer
	base    ai.Model
	h       harnessState
	g       guardState
	v3      toolRunState // harness v3 tool-side state (tool images, verification…)
}

// Run executes the loop. It returns ctx.Err() when cancelled, *Error for run failures.
func (a *Agent) Run(ctx context.Context, in Input, w eventlog.Writer) (Result, error) {
	ctx = llm.WithSession(ctx, in.Run.ThreadID) // every model call of the run (steps, compaction, judges)
	st := &runState{a: a, in: in, w: w, usage: agui.Usage{Model: in.Run.Model}}

	msgs, err := a.Store.ListMessages(ctx, in.Run.ThreadID)
	if err != nil {
		return Result{}, &Error{Code: agui.CodeInternal, Message: MsgInternal, Detail: "load messages: " + err.Error()}
	}
	st.loadHistory(ctx, msgs)
	st.resumeTurn(msgs)
	st.history = a.attachImages(ctx, in.Run, st.history)

	var toolList []ai.BaseTool
	cleanup := func() {}
	if a.Tools != nil {
		toolList, cleanup, err = a.Tools.Tools(ctx, tools.RunContext{
			ThreadID: in.Run.ThreadID, RunID: in.Run.ID, Login: in.Run.UserLogin, Sandbox: in.Run.Sandbox,
		})
		if err != nil {
			slog.Warn("agent: tools unavailable", "run_id", in.Run.ID, "error", err)
			toolList = nil
		}
		if cleanup == nil {
			cleanup = func() {}
		}
	}
	defer cleanup()

	plan := isPlanRun(in.Run)
	hasExec := false
	for _, t := range toolList {
		if info, err := t.Info(ctx); err == nil && info != nil && info.Name == "execute_command" {
			hasExec = true
		}
	}
	toolMap := map[string]ai.BaseTool{}
	var infos []*ai.ToolInfo
	for _, t := range toolList {
		info, err := t.Info(ctx)
		if err != nil || info == nil {
			continue
		}
		if plan && !planAllows(t, info.Name) {
			continue
		}
		toolMap[info.Name] = t
		infos = append(infos, info)
	}

	base, err := a.Models.Model(ctx, in.Run.Model)
	if err != nil {
		return Result{}, &Error{Code: agui.CodeModelError, Message: MsgModelError, Detail: err.Error()}
	}
	withTools := base
	if len(infos) > 0 {
		if withTools, err = base.WithTools(infos); err != nil {
			return Result{}, &Error{Code: agui.CodeInternal, Message: MsgInternal, Detail: "bind tools: " + err.Error()}
		}
	}
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	st.base = base
	st.g.guards = stopGuards(toolMap)
	st.observeTurn(toolMap)
	st.in.Sandbox = hasExec // a plan run keeps the sandbox prompt: it still reads the workspace
	st.buildPrompt(ctx, now())
	st.h.window = a.window(in.Run.Model)
	st.h.reserved = compaction.EstimateText(st.h.systemBase) + compaction.EstimateTools(infos)
	if in.Run.Kind == store.KindCompact {
		return st.compactRun(ctx, base)
	}
	st.beforeRun(ctx)
	if err := st.emitApprovalResolved(ctx); err != nil {
		return Result{}, err
	}
	if err := st.emitResumed(ctx); err != nil {
		return Result{}, err
	}
	// A resumed run first finishes the interrupted step (approved call, deferred calls).
	paused, err := st.resumeStep(ctx, msgs, toolMap)
	if ferr := w.Flush(ctx); ferr != nil {
		slog.Error("agent: event flush failed", "run_id", in.Run.ID, "error", ferr)
	}
	if err != nil {
		return Result{}, err
	}
	if paused {
		return Result{Usage: st.usages(), Interrupt: st.h.interrupt}, nil
	}

	maxSteps := max(a.MaxSteps, 1)
	pol := &runPolicy{st: st, m: withTools, hasTools: len(infos) > 0, toolMap: toolMap}
	switch _, err := aiagent.Loop(ctx, maxSteps, pol); {
	case err == nil:
		return Result{Usage: st.usages(), Interrupt: st.h.interrupt}, nil
	case errors.Is(err, aiagent.ErrStepLimit):
		return Result{}, &Error{Code: agui.CodeMaxSteps, Message: fmt.Sprintf("The agent did not finish within %d steps.", maxSteps)}
	default:
		return Result{}, err
	}
}

func (st *runState) emit(ctx context.Context, ev agui.Event) error {
	if _, err := st.w.Append(ctx, ev); err != nil {
		return &Error{Code: agui.CodeInternal, Message: MsgInternal, Detail: "event log: " + err.Error()}
	}
	return nil
}

func (st *runState) persist(ctx context.Context, msgs ...*store.Message) {
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	for _, m := range msgs {
		m.ThreadID, m.RunID = st.in.Run.ThreadID, st.in.Run.ID
		m.Clean()
	}
	if err := st.a.Store.AppendMessages(pctx, msgs...); err != nil {
		slog.Error("agent: persisting messages failed", "run_id", st.in.Run.ID, "error", err)
	}
}

// injectSteers adds pending steers to the history. On the final step it first
// closes the steer list, so a steer is either given to the model or refused.
func (st *runState) injectSteers(ctx context.Context, final bool) error {
	steers := st.carry
	st.carry = nil
	if st.a.Steers != nil {
		drain := st.a.Steers.DrainSteers
		if final {
			drain = st.a.Steers.FinalizeSteers
		}
		more, err := drain(ctx, st.in.Run.ID)
		if err != nil {
			slog.Warn("agent: draining steers failed", "run_id", st.in.Run.ID, "error", err)
		}
		steers = append(steers, more...)
	}
	if len(steers) > 0 {
		st.g.redirected()
	}
	for _, s := range steers {
		id := s.ID
		if id == "" {
			id = uuid.NewString()
		}
		row := &store.Message{ID: id, Role: store.RoleUser, Content: s.Text, Metadata: map[string]any{"steer": true, "delivered": true},
			AuthorLogin: s.Author, AuthorKind: s.AuthorKind, OnBehalfOf: s.OnBehalfOf}
		st.persist(ctx, row)
		hm := ai.UserMessage(SteerPrefix + AuthorTag(s.Author, s.AuthorKind, s.OnBehalfOf) + s.Text)
		compaction.SetPosition(hm, row.Position)
		compaction.MarkSteer(hm)
		st.history = append(st.history, hm)
		if err := st.emit(ctx, agui.SteerEventBy(id, s.Text, true, s.Author, s.AuthorKind, s.OnBehalfOf)); err != nil {
			return err
		}
	}
	return nil
}

func (st *runState) addUsage(msg *ai.Message) {
	if msg == nil || msg.ResponseMeta == nil || msg.ResponseMeta.Usage == nil {
		return
	}
	u := msg.ResponseMeta.Usage
	st.usage.CachedInputTokens += u.PromptTokenDetails.CachedTokens
	st.usage.InputTokens += u.PromptTokens
	st.usage.OutputTokens += u.CompletionTokens
	st.usage.TotalTokens += u.TotalTokens
	st.usage.ReasoningTokens += u.CompletionTokenDetails.ReasoningTokens
}

func toStoreCalls(calls []ai.ToolCall) []store.ToolCall {
	out := make([]store.ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, store.ToolCall{ID: c.ID, Type: "function", Function: store.ToolCallFunction{Name: c.Function.Name, Arguments: c.Function.Arguments}})
	}
	return out
}

func (st *runState) runTool(ctx context.Context, tc ai.ToolCall, toolMap map[string]ai.BaseTool) error {
	em := &toolEmitter{st: st, ctx: ctx}
	content, toolErr := st.execTool(ctx, tc, toolMap, em, st.g.looping(tc))
	return st.finishTool(ctx, tc, em, content, toolErr)
}

// execTool runs a call (or returns the loop guard's refusal: identical ×3,
// A-B-A-B-A-B): no events but the tool's own live output, no state change
// (read-only calls run it in parallel).
func (st *runState) execTool(ctx context.Context, tc ai.ToolCall, toolMap map[string]ai.BaseTool, em *toolEmitter, refusal string) (string, error) {
	if refusal != "" {
		return refusal, errors.New(refusal)
	}
	return st.invoke(ctx, tc, toolMap, em)
}

// finishTool emits, hooks, persists and records a call's result (in call order).
func (st *runState) finishTool(ctx context.Context, tc ai.ToolCall, em *toolEmitter, content string, toolErr error) error {
	if err := em.flushOutput(); err != nil {
		return err
	}
	if intr, ok := asInterrupt(toolErr); ok {
		var first bool
		if content, first = st.interrupted(tc, intr); first {
			return nil // no result: the user's answer becomes it
		}
		toolErr = errors.New(content)
	}
	cancelled := toolErr != nil && ctx.Err() != nil
	if cancelled {
		content, toolErr = cancelledResult, errors.New("cancelled")
	}
	if !cancelled {
		content = st.spill(ctx, tc, content)
	}
	content = CapOutput(content, st.a.MaxToolOutputChars)
	if untrustedResult(tc.Function.Name, content) {
		st.h.untrusted = true
	}
	st.recordSkill(tc.Function.Name, tc.ID, content, toolErr != nil)
	var metadata map[string]any
	if !cancelled {
		// Hooks queue their events (work.artifact, work.verification) behind
		// the result; their metadata (metadata.artifacts) is kept for reloads.
		// The checkers of changed deliverables run before the result is
		// emitted: their summary is part of it (harness v3 §4).
		note := st.g.observed(tc, content, toolErr != nil) // stuck detection (harness v3, 4.3)
		metadata = st.afterTool(ctx, tc, em)
		content, metadata = st.verifyAfterTool(ctx, tc, content, toolErr != nil, metadata, em)
		content += note
	}
	fields := map[string]any{"messageId": uuid.NewString(), "toolCallId": tc.ID, "content": content, "role": "tool"}
	errMsg := ""
	if toolErr != nil {
		fields["metadata"] = map[string]any{"isError": true}
		errMsg = truncate(toolErr.Error(), 2000)
	}
	if err := st.emit(ctx, agui.New(agui.ToolCallResult, fields)); err != nil {
		return err
	}
	row := &store.Message{ID: fields["messageId"].(string), Role: store.RoleTool, Content: content, ToolCallID: tc.ID, Error: errMsg, Metadata: metadata}
	st.persist(ctx, row)
	hm := ai.ToolMessage(content, tc.ID, ai.WithToolName(tc.Function.Name))
	compaction.SetPosition(hm, row.Position)
	compaction.SetFiles(hm, store.ArtifactPaths(row.Metadata))
	st.addToolResult(tc.ID, hm)
	if cancelled {
		return ctx.Err()
	}
	return em.flushAfter()
}

func (st *runState) invoke(ctx context.Context, tc ai.ToolCall, toolMap map[string]ai.BaseTool, em *toolEmitter) (string, error) {
	t, ok := toolMap[tc.Function.Name]
	if !ok {
		names := make([]string, 0, len(toolMap))
		for n := range toolMap {
			names = append(names, n)
		}
		sort.Strings(names)
		err := fmt.Errorf("unknown tool %q; available tools: %s", tc.Function.Name, strings.Join(names, ", "))
		return "Error: " + err.Error(), err
	}
	inv, ok := t.(ai.InvokableTool)
	if !ok {
		err := fmt.Errorf("tool %q cannot be invoked", tc.Function.Name)
		return "Error: " + err.Error(), err
	}
	args := strings.TrimSpace(tc.Function.Arguments)
	if args == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		err := fmt.Errorf("the arguments of %s are not valid JSON; send a single JSON object", tc.Function.Name)
		return "Error: " + err.Error(), err
	}
	tctx := tools.WithCall(ctx, tc.ID, em)
	out, err := aiagent.SafeInvoke(tctx, inv, args)
	if _, ok := asInterrupt(err); ok {
		return "", err
	}
	if err != nil {
		msg := err.Error()
		var ue *tools.UserError
		if errors.As(err, &ue) {
			msg = ue.Msg
		}
		return "Error: " + msg, errors.New(msg)
	}
	return out, nil
}

// CapOutput keeps the head and tail of s within max characters with a marker.
func CapOutput(s string, max int) string { return aiagent.CapOutput(s, max) }

func truncate(s string, n int) string { return aiagent.Truncate(s, n) }
