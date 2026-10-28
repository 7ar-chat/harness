// Package agent is the Work Chat agent loop: an own ReAct loop on eino
// ToolCallingChatModel that streams AG-UI events and checkpoints messages.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/agui"
	"github.com/GhaziBenDahmane/harness/auth"
	"github.com/GhaziBenDahmane/harness/compaction"
	"github.com/GhaziBenDahmane/harness/eventlog"
	"github.com/GhaziBenDahmane/harness/llm"
	"github.com/GhaziBenDahmane/harness/queue"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/tools"
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
	history []*schema.Message
	usage   agui.Usage
	carry   []queue.Steer
	base    model.ToolCallingChatModel
	h       harnessState
	g       guardState
	v3      v3State // harness v3 tool-side state (tool images, verification…)
}

// Run executes the loop. It returns ctx.Err() when cancelled, *Error for run failures.
func (a *Agent) Run(ctx context.Context, in Input, w eventlog.Writer) (Result, error) {
	ctx = auth.WithUserinfo(ctx, in.Run.Userinfo)
	st := &runState{a: a, in: in, w: w, usage: agui.Usage{Model: in.Run.Model}}

	msgs, err := a.Store.ListMessages(ctx, in.Run.ThreadID)
	if err != nil {
		return Result{}, &Error{Code: agui.CodeInternal, Message: MsgInternal, Detail: "load messages: " + err.Error()}
	}
	st.loadHistory(ctx, msgs)
	st.resumeTurn(msgs)
	st.history = a.attachImages(ctx, in.Run, st.history)

	var toolList []tool.BaseTool
	cleanup := func() {}
	if a.Tools != nil {
		toolList, cleanup, err = a.Tools.Tools(ctx, tools.RunContext{
			ThreadID: in.Run.ThreadID, RunID: in.Run.ID, Login: in.Run.UserLogin, Userinfo: in.Run.Userinfo,
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

	toolMap := map[string]tool.BaseTool{}
	var infos []*schema.ToolInfo
	for _, t := range toolList {
		info, err := t.Info(ctx)
		if err != nil || info == nil {
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
	st.in.Sandbox = toolMap["execute_command"] != nil
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

	maxSteps := a.MaxSteps
	if maxSteps < 1 {
		maxSteps = 1
	}
	for step := 1; step <= maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		done, err := st.step(ctx, step, step == maxSteps, withTools, len(infos) > 0, toolMap)
		if ferr := w.Flush(ctx); ferr != nil {
			slog.Error("agent: event flush failed", "run_id", in.Run.ID, "error", ferr)
		}
		if err != nil {
			return Result{}, err
		}
		if done {
			return Result{Usage: st.usages(), Interrupt: st.h.interrupt}, nil
		}
	}
	return Result{}, &Error{Code: agui.CodeMaxSteps, Message: fmt.Sprintf("The agent did not finish within %d steps.", maxSteps)}
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
		row := &store.Message{ID: id, Role: store.RoleUser, Content: s.Text, Metadata: map[string]any{"steer": true, "delivered": true}}
		st.persist(ctx, row)
		hm := schema.UserMessage(SteerPrefix + s.Text)
		compaction.SetPosition(hm, row.Position)
		compaction.MarkSteer(hm)
		st.history = append(st.history, hm)
		if err := st.emit(ctx, agui.SteerEvent(id, s.Text, true)); err != nil {
			return err
		}
	}
	return nil
}

func (st *runState) step(ctx context.Context, step int, last bool,
	m model.ToolCallingChatModel, hasTools bool, toolMap map[string]tool.BaseTool) (done bool, err error) {
	stepName := fmt.Sprintf("step-%d", step)
	if err := st.emit(ctx, agui.New(agui.StepStarted, map[string]any{"stepName": stepName})); err != nil {
		return false, err
	}
	stepClosed := false
	defer func() {
		// Failed or cancelled steps are still closed before the terminal event.
		if !stepClosed {
			_ = st.emit(ctx, agui.New(agui.StepFinished, map[string]any{"stepName": stepName}))
		}
	}()
	if err := st.injectSteers(ctx, last); err != nil {
		return false, err
	}
	if err := st.compact(ctx, st.base, compactAuto); err != nil {
		return false, err
	}
	forced := st.g.forceAnswer && !last // stuck on failing calls (guards.go)
	request := func() ([]*schema.Message, []model.Option) {
		msgs := st.requestMessages()
		opts := st.callOptions()
		if last || forced {
			// Keep the tool definitions (the history has tool messages) but forbid new calls.
			nudge := StepLimitNudge
			if forced {
				nudge = fmt.Sprintf(TooManyErrorsNote, st.g.failedInRow)
			}
			msgs = append(msgs, schema.UserMessage(nudge))
			if hasTools {
				opts = append(opts, model.WithToolChoice(schema.ToolChoiceForbidden))
			}
		}
		return msgs, opts
	}
	// Deliverables changed past the per-call check bound are checked now.
	if err := st.checkPending(ctx); err != nil {
		return false, err
	}
	st.budgetReminder(step)
	msgs, opts := request()
	callAt := len(st.history)
	out, err := st.callModel(ctx, m, msgs, opts...)
	if out == nil && llm.IsContextOverflow(err) && ctx.Err() == nil {
		// The prompt does not fit: compact (whatever the estimate) and retry once.
		compacted, cerr := st.compactOverflow(ctx)
		if cerr != nil {
			return false, cerr
		}
		if compacted {
			msgs, opts = request()
			callAt = len(st.history)
			out, err = st.callModel(ctx, m, msgs, opts...)
		}
	}
	if out != nil {
		st.addUsage(out.msg)
		st.observe(out.msg, callAt)
		st.imagesSent(msgs)
	}
	if out != nil && out.finishReason() == "content_filter" && err == nil {
		// The provider stopped the answer: nothing of it is kept.
		if cerr := st.closeAnnounced(ctx, out, ContentFilterResult); cerr != nil {
			return false, cerr
		}
		_ = st.emit(ctx, st.usageEvent())
		return false, &Error{Code: agui.CodeContentFilter, Message: MsgContentFilter, Detail: "finish_reason=content_filter"}
	}
	if out != nil {
		var rows []*store.Message
		if out.reasoning != "" {
			rows = append(rows, &store.Message{ID: out.reasoningID, Role: store.RoleReasoning, Content: out.reasoning})
		}
		var row *store.Message
		if out.msg.Content != "" || len(out.msg.ToolCalls) > 0 {
			row = &store.Message{ID: out.assistantID, Role: store.RoleAssistant, Content: out.msg.Content, ToolCalls: toStoreCalls(out.msg.ToolCalls),
				Metadata: st.answerMetadata(out.msg), ReasoningItems: llm.ReasoningItems(out.msg)}
			rows = append(rows, row)
		}
		st.persist(ctx, rows...)
		if err == nil {
			if verr := st.emitAnswerVerification(ctx, row); verr != nil {
				return false, verr
			}
		}
		if row != nil {
			hm := schema.AssistantMessage(out.msg.Content, out.msg.ToolCalls)
			if st.passback() {
				hm.ReasoningContent = out.reasoning
			}
			// Responses API: the encrypted reasoning goes back with its message (stream R).
			llm.CopyReasoningItems(hm, out.msg)
			compaction.SetPosition(hm, row.Position)
			st.history = append(st.history, hm)
		}
	}
	if err != nil {
		return false, err
	}

	finish := func() error {
		if err := st.emit(ctx, st.usageEvent()); err != nil {
			return err
		}
		stepClosed = true
		return st.emit(ctx, agui.New(agui.StepFinished, map[string]any{"stepName": stepName}))
	}

	st.g.noteFileIntent(out.msg.Content, out.msg.ToolCalls)
	if len(out.msg.ToolCalls) == 0 {
		st.h.answer = out.msg.Content
		empty := strings.TrimSpace(out.msg.Content) == ""
		if empty {
			// A refusal or an empty completion: one nudge, then empty_answer (never a silent success).
			st.h.emptyAnswers++
		} else {
			st.h.emptyAnswers = 0
		}
		if err := finish(); err != nil {
			return false, err
		}
		if empty && (last || st.h.emptyAnswers >= 2 || ctx.Err() != nil) {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, &Error{Code: agui.CodeEmptyAnswer, Message: MsgEmptyAnswer, Detail: emptyDetail(out)}
		}
		if empty && st.h.interrupt == nil {
			st.g.nudged = true
			st.g.redirected()
			st.history = append(st.history, schema.UserMessage(EmptyAnswerNudge))
			return false, nil
		}
		if last {
			if st.missingFile() {
				// The budget ran out without the file the run was writing: the
				// forced answer is kept, but the run is not a success (A2, F3).
				return false, &Error{Code: agui.CodeMaxSteps, Message: MsgStepLimitNoDeliverable}
			}
			return true, nil
		}
		if out.finishReason() == "length" && st.h.continuations < maxContinuations {
			// The answer was cut by the output limit: let the model finish it.
			st.h.continuations++
			st.g.redirected()
			st.history = append(st.history, schema.UserMessage(ContinueAfterLengthNote))
			return false, nil
		}
		// Decide whether the run goes on before closing the steer list, so a
		// steer is only refused by a run that really stops.
		if note := st.continueAfterAnswer(ctx, out.msg.Content); note != "" {
			st.history = append(st.history, schema.UserMessage(note))
			return false, nil
		}
		if st.a.Steers != nil {
			// Stop accepting steers, then drain: a steer that arrived during the
			// final answer gets its own step (and steers are accepted again).
			pending, ferr := st.a.Steers.FinalizeSteers(ctx, st.in.Run.ID)
			if ferr != nil {
				slog.Warn("agent: finalizing steers failed", "run_id", st.in.Run.ID, "error", ferr)
			}
			if len(pending) > 0 {
				if rerr := st.a.Steers.ReopenSteers(ctx, st.in.Run.ID); rerr != nil {
					slog.Warn("agent: reopening steers failed", "run_id", st.in.Run.ID, "error", rerr)
				}
				st.carry = pending
				return false, nil
			}
		}
		return true, nil
	}
	st.h.emptyAnswers = 0
	if last {
		_ = finish()
		return false, &Error{Code: agui.CodeMaxSteps, Message: "The agent reached its step limit before finishing."}
	}
	if out.finishReason() == "length" {
		// The calls were cut by the output limit: their arguments are incomplete, never run them.
		for _, tc := range out.msg.ToolCalls {
			if err := st.refuseTool(ctx, tc, OutputLimitResult); err != nil {
				return false, err
			}
		}
		st.g.redirected()
		return false, finish()
	}

	st.g.called += len(out.msg.ToolCalls)
	if err := st.runTools(ctx, out.msg.ToolCalls, toolMap); err != nil {
		return false, err
	}
	// A tool needs the user (ask_user, a risky call in interactive mode): the
	// run ends here, the answer or decision resumes it.
	return st.h.interrupt != nil, finish()
}

func (st *runState) addUsage(msg *schema.Message) {
	if msg == nil || msg.ResponseMeta == nil || msg.ResponseMeta.Usage == nil {
		return
	}
	u := msg.ResponseMeta.Usage
	st.usage.CachedInputTokens += u.PromptTokenDetails.CachedTokens
	st.usage.InputTokens += u.PromptTokens
	st.usage.OutputTokens += u.CompletionTokens
	st.usage.TotalTokens += u.TotalTokens
	st.usage.ReasoningTokens += u.CompletionTokensDetails.ReasoningTokens
}

func toStoreCalls(calls []schema.ToolCall) []store.ToolCall {
	out := make([]store.ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, store.ToolCall{ID: c.ID, Type: "function", Function: store.ToolCallFunction{Name: c.Function.Name, Arguments: c.Function.Arguments}})
	}
	return out
}

func (st *runState) runTool(ctx context.Context, tc schema.ToolCall, toolMap map[string]tool.BaseTool) error {
	em := &toolEmitter{st: st, ctx: ctx}
	content, toolErr := st.execTool(ctx, tc, toolMap, em, st.g.looping(tc))
	return st.finishTool(ctx, tc, em, content, toolErr)
}

// execTool runs a call (or returns the loop guard's refusal: identical ×3,
// A-B-A-B-A-B): no events but the tool's own live output, no state change
// (read-only calls run it in parallel).
func (st *runState) execTool(ctx context.Context, tc schema.ToolCall, toolMap map[string]tool.BaseTool, em *toolEmitter, refusal string) (string, error) {
	if refusal != "" {
		return refusal, errors.New(refusal)
	}
	return st.invoke(ctx, tc, toolMap, em)
}

// finishTool emits, hooks, persists and records a call's result (in call order).
func (st *runState) finishTool(ctx context.Context, tc schema.ToolCall, em *toolEmitter, content string, toolErr error) error {
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
	hm := schema.ToolMessage(content, tc.ID, schema.WithToolName(tc.Function.Name))
	compaction.SetPosition(hm, row.Position)
	compaction.SetFiles(hm, store.ArtifactPaths(row.Metadata))
	st.addToolResult(tc.ID, hm)
	if cancelled {
		return ctx.Err()
	}
	return em.flushAfter()
}

func (st *runState) invoke(ctx context.Context, tc schema.ToolCall, toolMap map[string]tool.BaseTool, em *toolEmitter) (string, error) {
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
	inv, ok := t.(tool.InvokableTool)
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
	out, err := safeInvoke(tctx, inv, args)
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

func safeInvoke(ctx context.Context, t tool.InvokableTool, args string) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("the tool crashed: %v", r)
		}
	}()
	return t.InvokableRun(ctx, args)
}

// CapOutput keeps the head and tail of s within max characters with a marker.
func CapOutput(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	marker := fmt.Sprintf("\n\n[... %d characters truncated ...]\n\n", len(r)-max)
	keep := max - utf8.RuneCountInString(marker)
	if keep < 0 {
		keep = 0
	}
	head := keep * 2 / 3
	tail := keep - head
	return string(r[:head]) + marker + string(r[len(r)-tail:])
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
