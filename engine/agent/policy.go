package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/7ar-chat/harness/ai"
	aiagent "github.com/7ar-chat/harness/ai/agent"
	"github.com/7ar-chat/harness/ai/compaction"
	"github.com/7ar-chat/harness/ai/llm"
	"github.com/7ar-chat/harness/ai/llm/responses"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
)

// runPolicy is the aiagent.Policy of a persisted run: the conversation is st.history, every step is bracketed by
// AG-UI step events, answers and tool results are stored, and the decisions about when a run is over (empty answers,
// output limits, deliverables, steers that arrived late) are the harness's own.
type runPolicy struct {
	aiagent.Base
	st       *runState
	m        ai.Model
	hasTools bool
	toolMap  map[string]ai.BaseTool

	// Per step.
	stepName string
	started  bool // STEP_STARTED was emitted
	closed   bool // STEP_FINISHED was emitted
	forced   bool // stuck on failing calls: this step answers without tools (guards.go)
	callAt   int  // the history length when the request was built
	out      *stepOutput
}

var _ aiagent.Policy = (*runPolicy)(nil)

// BeginStep opens the step and does the work that precedes the request: steers, compaction, deliverable checks and
// notes.
func (p *runPolicy) BeginStep(ctx context.Context, s *aiagent.Step) error {
	st := p.st
	p.stepName = fmt.Sprintf("step-%d", s.N)
	p.started, p.closed, p.out = false, false, nil
	if err := st.emit(ctx, agui.New(agui.StepStarted, map[string]any{"stepName": p.stepName})); err != nil {
		return err
	}
	p.started = true
	if err := st.injectSteers(ctx, s.Last); err != nil {
		return err
	}
	if err := st.compact(ctx, st.base, compactAuto); err != nil {
		return err
	}
	p.forced = st.g.forceAnswer && !s.Last
	// Deliverables changed past the per-call check bound are checked now.
	if err := st.checkPending(ctx); err != nil {
		return err
	}
	st.stepNotes(ctx)
	st.budgetReminder(s.N)
	return nil
}

// Request builds the model input; on the last step, or when forced, it keeps the tool definitions (the history has
// tool messages) but forbids new calls.
func (p *runPolicy) Request(_ context.Context, s *aiagent.Step) ([]*ai.Message, []ai.Option) {
	st := p.st
	msgs := st.requestMessages()
	opts := st.callOptions()
	if s.Last || p.forced {
		nudge := StepLimitNudge
		if p.forced {
			nudge = fmt.Sprintf(TooManyErrorsNote, st.g.failedInRow)
		}
		msgs = append(msgs, ai.UserMessage(nudge))
		if p.hasTools {
			opts = append(opts, ai.WithToolChoice(ai.ToolChoiceForbidden))
		}
	}
	p.callAt = len(st.history)
	return msgs, opts
}

// Call makes the model call: streamed as AG-UI events, under the watchdog, retried when it is worth it.
func (p *runPolicy) Call(ctx context.Context, s *aiagent.Step) (*ai.Message, error) {
	out, err := p.st.callModel(ctx, p.m, s.Messages, s.Options...)
	p.out = out
	if out == nil {
		return nil, err
	}
	return out.msg, err
}

// Recover compacts after a context overflow so the same step can be asked again, once.
func (p *runPolicy) Recover(ctx context.Context, _ *aiagent.Step, err error) (bool, error) {
	if !llm.IsContextOverflow(err) {
		return false, nil
	}
	// The prompt does not fit: compact (whatever the estimate) and retry once.
	return p.st.compactOverflow(ctx)
}

// Replied counts the usage, applies the provider's content filter and stores the answer.
func (p *runPolicy) Replied(ctx context.Context, s *aiagent.Step, _ *ai.Message, callErr error) error {
	st, out := p.st, p.out
	if out == nil {
		return nil
	}
	st.addUsage(out.msg)
	st.observe(out.msg, p.callAt)
	st.imagesSent(s.Messages)
	if out.finishReason() == "content_filter" && callErr == nil {
		// The provider stopped the answer: nothing of it is kept.
		if cerr := st.closeAnnounced(ctx, out, ContentFilterResult); cerr != nil {
			return cerr
		}
		_ = st.emit(ctx, st.usageEvent())
		return &Error{Code: agui.CodeContentFilter, Message: MsgContentFilter, Detail: "finish_reason=content_filter"}
	}
	var rows []*store.Message
	if out.reasoning != "" {
		rows = append(rows, &store.Message{ID: out.reasoningID, Role: store.RoleReasoning, Content: out.reasoning})
	}
	var row *store.Message
	if out.msg.Content != "" || len(out.msg.ToolCalls) > 0 {
		row = &store.Message{ID: out.assistantID, Role: store.RoleAssistant, Content: out.msg.Content, ToolCalls: toStoreCalls(out.msg.ToolCalls),
			Metadata: st.answerMetadata(out.msg), ReasoningItems: responses.MarshalItems(responses.ItemsOf(out.msg))}
		rows = append(rows, row)
	}
	st.persist(ctx, rows...)
	if callErr == nil {
		if verr := st.emitAnswerVerification(ctx, row); verr != nil {
			return verr
		}
	}
	if row != nil {
		hm := ai.AssistantMessage(out.msg.Content, out.msg.ToolCalls)
		if st.passback() {
			hm.ReasoningContent = out.reasoning
		}
		// Responses API: the encrypted reasoning goes back with its message (stream R).
		responses.SetItems(hm, responses.ItemsOf(out.msg))
		compaction.SetPosition(hm, row.Position)
		st.history = append(st.history, hm)
	}
	return nil
}

// finish closes the step: usage, then STEP_FINISHED. It is called at the point of each path where the step is over;
// EndStep covers the paths that fail first.
func (p *runPolicy) finish(ctx context.Context) error {
	if err := p.st.emit(ctx, p.st.usageEvent()); err != nil {
		return err
	}
	p.closed = true
	return p.st.emit(ctx, agui.New(agui.StepFinished, map[string]any{"stepName": p.stepName}))
}

// Next decides what an answer means: an answer without calls ends the run unless it is empty, was cut, leaves a
// deliverable missing or a steer arrived; calls are run, unless they were cut by the output limit.
func (p *runPolicy) Next(ctx context.Context, s *aiagent.Step, _ *ai.Message) (aiagent.Action, error) {
	st, out, last := p.st, p.out, s.Last
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
		if err := p.finish(ctx); err != nil {
			return aiagent.Finish, err
		}
		if empty && (last || st.h.emptyAnswers >= 2 || ctx.Err() != nil) {
			if ctx.Err() != nil {
				return aiagent.Finish, ctx.Err()
			}
			return aiagent.Finish, &Error{Code: agui.CodeEmptyAnswer, Message: MsgEmptyAnswer, Detail: emptyDetail(out)}
		}
		if empty && st.h.interrupt == nil {
			st.g.nudged = true
			st.g.redirected()
			st.history = append(st.history, ai.UserMessage(EmptyAnswerNudge))
			return aiagent.Continue, nil
		}
		if last {
			if st.missingFile() {
				// The budget ran out without the file the run was writing: the
				// forced answer is kept, but the run is not a success (A2, F3).
				return aiagent.Finish, &Error{Code: agui.CodeMaxSteps, Message: MsgStepLimitNoDeliverable}
			}
			return aiagent.Finish, nil
		}
		if out.finishReason() == "length" && st.h.continuations < maxContinuations {
			// The answer was cut by the output limit: let the model finish it.
			st.h.continuations++
			st.g.redirected()
			st.history = append(st.history, ai.UserMessage(ContinueAfterLengthNote))
			return aiagent.Continue, nil
		}
		// Decide whether the run goes on before closing the steer list, so a
		// steer is only refused by a run that really stops.
		if note := st.continueAfterAnswer(ctx, out.msg.Content); note != "" {
			st.history = append(st.history, ai.UserMessage(note))
			return aiagent.Continue, nil
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
				return aiagent.Continue, nil
			}
		}
		return aiagent.Finish, nil
	}
	st.h.emptyAnswers = 0
	if last {
		_ = p.finish(ctx)
		return aiagent.Finish, &Error{Code: agui.CodeMaxSteps, Message: "The agent reached its step limit before finishing."}
	}
	if out.finishReason() == "length" {
		// The calls were cut by the output limit: their arguments are incomplete, never run them.
		for _, tc := range out.msg.ToolCalls {
			if err := st.refuseTool(ctx, tc, OutputLimitResult); err != nil {
				return aiagent.Finish, err
			}
		}
		st.g.redirected()
		return aiagent.Continue, p.finish(ctx)
	}
	st.g.called += len(out.msg.ToolCalls)
	return aiagent.RunTools, nil
}

// Tools runs the step's calls. A tool that needs the user (ask_user, a risky call in interactive mode) ends the run
// here; the answer or decision resumes it.
func (p *runPolicy) Tools(ctx context.Context, _ *aiagent.Step, calls []ai.ToolCall) (bool, error) {
	if err := p.st.runTools(ctx, calls, p.toolMap); err != nil {
		return false, err
	}
	return p.st.h.interrupt != nil, p.finish(ctx)
}

// EndStep closes a step that failed or was cancelled before its own close, then flushes the step's events.
func (p *runPolicy) EndStep(ctx context.Context, _ *aiagent.Step) {
	if p.started && !p.closed {
		// Failed or cancelled steps are still closed before the terminal event.
		_ = p.st.emit(ctx, agui.New(agui.StepFinished, map[string]any{"stepName": p.stepName}))
		p.closed = true
	}
	if ferr := p.st.w.Flush(ctx); ferr != nil {
		slog.Error("agent: event flush failed", "run_id", p.st.in.Run.ID, "error", ferr)
	}
}
