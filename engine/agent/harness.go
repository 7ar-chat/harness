package agent

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/compaction"
	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/modelinfo"
	"github.com/GhaziBenDahmane/harness/prompts"
)

// SummaryStore persists the thread's compaction summary.
type SummaryStore interface {
	GetThreadSummary(ctx context.Context, threadID string) (store.ThreadSummary, error)
	SetThreadSummary(ctx context.Context, threadID string, s store.ThreadSummary) error
}

// CompactionStore records completed compactions.
type CompactionStore interface {
	RecordCompaction(ctx context.Context, c store.Compaction) error
}

// Harness holds the harness v2 options of the agent (all optional).
type Harness struct {
	// Sections extend the system prompt (skills, file references, memory, goal,
	// plan instructions…), in order.
	Sections []PromptSection
	// Context sections are dynamic (goal, recalled memories): rendered in the
	// prompt's final <context> block, after the static sections.
	Context []PromptSection
	// Tail sections change from run to run of a thread (the goal's turn
	// counter): rendered once per run into a reminder at the end of every
	// request, after the history, so the system prompt and the history before
	// it stay a cacheable prefix across runs (review round 1).
	Tail []PromptSection
	// StepNotes add mid-run notes before a step's model call (see StepNote): news the prompt, rendered at
	// the start of the run, could not carry.
	StepNotes []StepNote
	// PromptVariant is the prompt variant of a model (LLM_PROMPT_VARIANTS;
	// nil: prompts.Default).
	PromptVariant func(model string) string
	// ToolHooks observe tool results (artifact snapshots).
	ToolHooks []ToolHook
	// RunStart hooks run before the first step of every run but compactions
	// (the workspace checkpoint "Before AI turn").
	RunStart []RunStartHook
	// Images adds attached images as image parts for vision models.
	Images ImageAttacher
	// Checker runs the checker of a deliverable changed by a tool call (harness
	// v3 §4: post-write validation, verification StopGuard, metadata.verification).
	Checker Checker
	// Vision reports models that accept images (LLM_VISION_MODELS): the images
	// tools attach (tools.AttachImage: read_file, view_file) only reach those.
	Vision func(model string) bool
	// Summaries enables persisted compaction summaries.
	Summaries SummaryStore
	// Compactions records completed compactions (the UI's divider after a reload).
	Compactions CompactionStore
	// ContextWindow is a model's window in tokens (default 128000).
	ContextWindow func(model string) int
	// CompactionThreshold is the share of the window that triggers compaction
	// (0: window − OutputReserve, harness v3 §5).
	CompactionThreshold float64
	// Plans and Deliverables feed the context restored after a compaction
	// (harness v3 §5; nil: the plan is read from the history, no deliverables).
	Plans        PlanSource
	Deliverables DeliverableSource
	// Spill writes tool outputs longer than Agent.SpillChars to a workspace file (nil: they are cut).
	Spill tools.OutputSpiller
	// KeepRecentToolResults: tool results older than the newest K are masked in
	// the model input (COMPACTION_KEEP_RECENT_TOOL_RESULTS; 0: 6; < 0: no masking).
	KeepRecentToolResults int
	// MaskFromFraction: masking starts only once the prompt passes this share of
	// the window (COMPACTION_MASK_FROM_FRACTION; 0: 0.5; < 0: from the first
	// step, the v3 batches of min(4, keep)). Round 3 (A1).
	MaskFromFraction float64
	// OutputReserve is the part of a model's window kept free below the
	// compaction trigger: max output tokens + 20k (nil: 36000).
	OutputReserve func(model string) int
	// CompactionModel summarizes old context (default: the run's model).
	CompactionModel string
	// SupportsReasoning reports models that accept reasoningEffort.
	SupportsReasoning func(model string) bool
	// ReasoningEffort maps a run's effort to the one sent to its model (the
	// nearest the model accepts, "" for none; nil: sent as is). A-29.
	ReasoningEffort func(model, effort string) string
	// PromptCacheKey reports models whose calls send prompt_cache_key (the thread
	// id) so the provider routes a thread's calls to the same prompt cache.
	PromptCacheKey func(model string) bool
	// ReasoningPassback reports models whose assistant messages carry their
	// reasoning_content back, within and across runs (LLM_REASONING_PASSBACK_MODELS).
	ReasoningPassback func(model string) bool
	// Answers reviews the final answer of a run that still has steps left
	// (dangling file links); a note gets one corrective step.
	Answers AnswerChecker
	// BeforeTool gates tool calls before they run (interactive approvals);
	// Approvals gives a resumed run its decided approval.
	BeforeTool []BeforeToolHook
	Approvals  ApprovalSource
}

const (
	summaryTimeout = 180 * time.Second // per summarizer call (chunks up to compaction.MaxChunkTokens)
	// SummaryHeading introduces the compaction summary in the system prompt.
	SummaryHeading = prompts.SummaryHeading
	// TriggerAuto / TriggerManual are work.compaction triggers.
	TriggerAuto   = "auto"
	TriggerManual = "manual"
	// OneQuestionResult answers a second ask_user call of the same step.
	OneQuestionResult = "Error: ask only one question at a time; wait for the answer to the first one."
)

func (a *Agent) window(m string) int {
	if a.Harness.ContextWindow != nil {
		if n := a.Harness.ContextWindow(m); n > 0 {
			return n
		}
	}
	return modelinfo.DefaultContextWindow
}

// harnessState is the per-run state of the harness features.
type harnessState struct {
	// prompt is the run's system prompt without the summary; systemBase its text.
	prompt     prompts.Prompt
	systemBase string
	// tail is the run's rendered Tail sections ("" when none).
	tail     string
	summary  string
	reserved int // system prompt (without summary) + tools, in tokens
	window   int
	// lastPrompt is the prompt tokens reported by the previous call (0: unknown),
	// callAt the history length when it was made.
	lastPrompt  int
	callAt      int
	lastContext int
	resumed     []*store.Message
	interrupt   *tools.Interrupt
	// answer is the text of the last answer without tool calls; answerChecked
	// is set once the answer checker ran (at most once per run).
	answer        string
	answerChecked bool
	// untrusted is set once the model's context holds untrusted content.
	untrusted  bool
	extraUsage map[string]*agui.Usage
	usageMu    sync.Mutex // guards extraUsage (sub-agent usage arrives from parallel tool calls)
	summarizer ai.ChatModel
	// Approvals: the approval this run resumes, whether the run carries its own
	// user message, and the history slots of calls finished by this run.
	approval   *store.Approval
	ownMessage bool
	slots      map[string]int
	// regrowAt: after a compaction, the next threshold compaction waits until
	// the history (estimated, without system prompt) reaches this size.
	regrowAt int
	// compacted is set when the last compaction check changed the history.
	compacted bool
	// emptyAnswers counts consecutive empty answers (refusals included);
	// continuations the answers continued after finish_reason=length.
	emptyAnswers  int
	continuations int
	// images are the images the step's tools attached (tools.AttachImage).
	images imageQueue
	// skills are the load_skill results of this run; rehydration is the context
	// message restored after the last compaction (sent after the system prompt).
	skills      []loadedSkill
	rehydration *ai.Message
	// maskedCalls are the tool call ids whose results the model input masks
	// (set once the prompt passed Harness.MaskFromFraction × window).
	maskedCalls map[string]bool
}

// Compaction modes: before a model call, a compact run, after a context overflow.
const (
	compactAuto = iota
	compactManual
	compactOverflow
)

// loadHistory builds the model history: the stored summary plus the messages after it.
func (st *runState) loadHistory(ctx context.Context, msgs []*store.Message) {
	var through int64
	if s := st.a.Harness.Summaries; s != nil {
		sum, err := s.GetThreadSummary(ctx, st.in.Run.ThreadID)
		if err != nil {
			slog.Warn("agent: loading the context summary failed", "run_id", st.in.Run.ID, "error", err)
		} else if sum.Text != "" {
			st.h.summary, through = sum.Text, sum.ThroughPosition
		}
	}
	var after []*store.Message
	for _, m := range msgs {
		if m.Position > through {
			after = append(after, m)
		}
		if m.RunID == st.in.Run.ID && m.Role == store.RoleTool {
			st.h.resumed = append(st.h.resumed, m)
		}
		if m.RunID == st.in.Run.ID && m.Role == store.RoleUser && !m.IsSteer() {
			st.h.ownMessage = true
		}
	}
	st.history = BuildHistoryWith(after, HistoryOptions{Reasoning: st.passback()})
	st.h.untrusted = st.h.summary != "" || untrustedHistory(st.history)
}

// trustedTools return no content from files, commands or third parties (the
// memory tools return only the user's own confirmed memory).
var trustedTools = map[string]bool{"update_plan": true, "ask_user": true, "remember": true, "forget": true, "write_file": true,
	"memory_search": true, "memory_get": true, "memory_create": true, "memory_update": true, "memory_delete": true,
	"memory_create_batch": true, "memory_delete_batch": true, "memory_capabilities": true}

// userSkillMarker starts the fenced text of a user skill (skills.Fence).
const userSkillMarker = `<skill source="user"`

// untrustedResult reports tool results that carry untrusted content.
func untrustedResult(tool, content string) bool {
	if tool == "load_skill" {
		return strings.Contains(content, userSkillMarker)
	}
	return !trustedTools[tool]
}

func untrustedHistory(history []*ai.Message) bool {
	for _, m := range history {
		switch m.Role {
		case ai.RoleTool:
			if untrustedResult(m.ToolName, m.Content) {
				return true
			}
		case ai.RoleUser:
			if _, ok := m.Extra[store.ExtraAttachments]; ok {
				return true
			}
		}
	}
	return false
}

// Untrusted implements tools.UntrustedSource.
func (e *toolEmitter) Untrusted() bool { return e.st.h.untrusted }

// emitResumed replays the tool results stored when the run was created (ask_user answers).
func (st *runState) emitResumed(ctx context.Context) error {
	for _, m := range st.h.resumed {
		if err := st.emit(ctx, agui.New(agui.ToolCallResult, map[string]any{
			"messageId": m.ID, "toolCallId": m.ToolCallID, "content": m.Content, "role": "tool",
		})); err != nil {
			return err
		}
	}
	return nil
}

// system is the system prompt of the next call: the run's prompt with the
// current summary in its dynamic context (prompts.Prompt.Render).
func (st *runState) system() *ai.Message {
	return ai.SystemMessage(st.h.prompt.Render(st.h.summary))
}

// FenceData wraps untrusted text in <tag>…</tag>, escaping every opening or
// closing form of the tag inside it (any case, whitespace around "/") so the
// text cannot close the block nor open a nested one.
func FenceData(tag, text string) string { return prompts.FenceData(tag, text) }

// callOptions are the per-call model options of the run (reasoning effort,
// prompt cache key = the thread id).
func (st *runState) callOptions() []ai.Option {
	var opts []ai.Option
	if f := st.a.Harness.PromptCacheKey; f != nil && f(st.in.Run.Model) {
		opts = append(opts, ai.WithPromptCacheKey(st.in.Run.ThreadID))
	}
	effort := st.in.Run.ReasoningEffort
	if effort == "" || st.a.Harness.SupportsReasoning == nil || !st.a.Harness.SupportsReasoning(st.in.Run.Model) {
		return opts
	}
	if f := st.a.Harness.ReasoningEffort; f != nil {
		if effort = f(st.in.Run.Model, effort); effort == "" {
			return opts
		}
	}
	return append(opts, ai.WithReasoningEffort(effort))
}

// observe records the prompt size reported for a call made with history[:callAt].
func (st *runState) observe(msg *ai.Message, callAt int) {
	st.h.callAt = callAt
	st.h.lastPrompt = 0
	if msg != nil && msg.ResponseMeta != nil && msg.ResponseMeta.Usage != nil {
		st.h.lastPrompt = msg.ResponseMeta.Usage.PromptTokens
	}
	if st.h.lastPrompt > 0 {
		st.h.lastContext = st.h.lastPrompt
	}
}

func (st *runState) observed() int {
	if st.h.lastPrompt <= 0 || st.h.callAt > len(st.history) {
		return 0
	}
	return st.h.lastPrompt + compaction.EstimateMessages(st.history[st.h.callAt:])
}

func (st *runState) summarizerModel(ctx context.Context, base ai.ChatModel) ai.ChatModel {
	if st.h.summarizer != nil {
		return st.h.summarizer
	}
	st.h.summarizer, st.v3.summarizer = base, st.in.Run.Model
	if name := st.a.Harness.CompactionModel; name != "" && name != st.in.Run.Model {
		if m, err := st.a.Models.Model(ctx, name); err == nil {
			st.h.summarizer, st.v3.summarizer = m, name
		} else {
			slog.Warn("agent: compaction model unavailable, using the run's model", "model", name, "error", err)
		}
	}
	return st.h.summarizer
}

// summarizerName is the model that actually summarizes (the run's model when
// COMPACTION_MODEL is unavailable: its usage is counted under that name, A-25).
func (st *runState) summarizerName() string {
	if st.v3.summarizer != "" {
		return st.v3.summarizer
	}
	return st.in.Run.Model
}

func (st *runState) addModelUsage(name string, msg *ai.Message) {
	if name == st.in.Run.Model {
		st.addUsage(msg)
		return
	}
	if msg == nil || msg.ResponseMeta == nil || msg.ResponseMeta.Usage == nil {
		return
	}
	st.h.usageMu.Lock()
	defer st.h.usageMu.Unlock()
	if st.h.extraUsage == nil {
		st.h.extraUsage = map[string]*agui.Usage{}
	}
	u := st.h.extraUsage[name]
	if u == nil {
		u = &agui.Usage{Model: name}
		st.h.extraUsage[name] = u
	}
	r := msg.ResponseMeta.Usage
	u.InputTokens += r.PromptTokens
	u.OutputTokens += r.CompletionTokens
	u.TotalTokens += r.TotalTokens
	u.ReasoningTokens += r.CompletionTokenDetails.ReasoningTokens
	u.CachedInputTokens += r.PromptTokenDetails.CachedTokens
}

// AddSubagentUsage implements tools.LiveEmitter: the sub-agent's usage joins the
// run's per-model totals under "<model> (sub-agent)", so it is neither lost nor
// mixed into the main loop's counters (tools may run in parallel).
func (e *toolEmitter) AddSubagentUsage(model string, in, out, cached int) {
	h := &e.st.h
	h.usageMu.Lock()
	defer h.usageMu.Unlock()
	name := model + " (sub-agent)"
	if h.extraUsage == nil {
		h.extraUsage = map[string]*agui.Usage{}
	}
	u := h.extraUsage[name]
	if u == nil {
		u = &agui.Usage{Model: name}
		h.extraUsage[name] = u
	}
	u.InputTokens += in
	u.OutputTokens += out
	u.TotalTokens += in + out
	u.CachedInputTokens += cached
}

func (st *runState) usages() []agui.Usage {
	st.h.usageMu.Lock()
	defer st.h.usageMu.Unlock()
	out := []agui.Usage{st.usage}
	for _, u := range st.h.extraUsage {
		out = append(out, *u)
	}
	return out
}

// summarize is the compaction model call.
func (st *runState) summarize(base ai.ChatModel) compaction.Summarizer {
	return func(ctx context.Context, previous, transcript string) (string, error) {
		m := st.summarizerModel(ctx, base)
		ctx, cancel := context.WithTimeout(ctx, summaryTimeout)
		defer cancel()
		var prompt strings.Builder
		if previous != "" {
			prompt.WriteString("Previous summary (merge it):\n\n" + previous + "\n\n")
		}
		prompt.WriteString("Conversation part to summarize:\n\n" + transcript)
		var opts []ai.Option
		if f := st.a.Harness.SupportsReasoning; f != nil && f(st.summarizerName()) {
			opts = []ai.Option{ai.WithReasoningEffort("low")} // auxiliary call (A-25)
		}
		out, err := m.Generate(ctx, []*ai.Message{ai.SystemMessage(compaction.Instructions), ai.UserMessage(prompt.String())}, opts...)
		if err != nil {
			return "", err
		}
		st.addModelUsage(st.summarizerName(), out)
		return out.Content, nil
	}
}

// summaryBudget is the input the summarizing model's window leaves for the
// previous summary and the transcript, in tokens.
func (st *runState) summaryBudget() int {
	name := st.a.Harness.CompactionModel
	if name == "" {
		name = st.in.Run.Model
	}
	return max(st.a.window(name)-compaction.EstimateText(compaction.Instructions)-compaction.SummaryOutputTokens-200, 1000)
}

// compactOverflow compacts after a context overflow; it reports whether the
// history changed (then the call is retried once).
func (st *runState) compactOverflow(ctx context.Context) (bool, error) {
	if err := st.compact(ctx, st.base, compactOverflow); err != nil {
		return false, err
	}
	return st.h.compacted, nil
}

// defaultOutputReserve is the reserve of models without OutputReserve: 16k output + 20k.
const defaultOutputReserve = 36000

// threshold is the share of the window that triggers compaction: the
// configured CompactionThreshold, else (window − reserve) / window, where the
// reserve is the model's max output tokens + 20k (harness v3 §5, A-02).
func (st *runState) threshold() float64 {
	if t := st.a.Harness.CompactionThreshold; t > 0 && t < 1 {
		return t
	}
	reserve := defaultOutputReserve
	if f := st.a.Harness.OutputReserve; f != nil {
		if r := f(st.in.Run.Model); r > 0 {
			reserve = r
		}
	}
	if st.h.window <= 0 {
		return 0.75
	}
	return min(max(float64(st.h.window-reserve)/float64(st.h.window), 0.3), 0.95)
}

// compact runs the compaction check before a model call (or for a manual compact run).
// A cancellation while summarizing emits phase "cancelled" (A-20) and returns
// ctx.Err(): the run ends cancelled.
func (st *runState) compact(ctx context.Context, base ai.ChatModel, mode int) error {
	trigger := TriggerAuto
	if mode == compactManual {
		trigger = TriggerManual
	}
	st.h.compacted = false
	if mode == compactAuto && st.h.regrowAt > 0 && compaction.EstimateMessages(st.history) < st.h.regrowAt {
		return nil // compacted recently: wait until the history has grown
	}
	started, startedBefore := false, 0
	res, err := compaction.Compact(ctx, st.history, compaction.Options{
		Window:        st.h.window,
		Threshold:     st.threshold(),
		Reserved:      st.h.reserved + st.rehydrationTokens() + compaction.EstimateText(st.h.tail),
		Observed:      st.observed(),
		Previous:      st.h.summary,
		Summarize:     st.summarize(base),
		SummaryBudget: st.summaryBudget(),
		Manual:        mode == compactManual,
		Force:         mode == compactOverflow,
		OnStart: func(before int) {
			started, startedBefore = true, before
			_ = st.emit(ctx, agui.CustomEvent(agui.CustomCompaction, map[string]any{"phase": "started", "trigger": trigger, "tokensBefore": before}))
		},
	})
	if err != nil {
		if started {
			// The emit is detached from the cancelled context (event log).
			_ = st.emit(ctx, agui.CustomEvent(agui.CustomCompaction, map[string]any{"phase": "cancelled", "trigger": trigger, "tokensBefore": startedBefore}))
		}
		return err
	}
	st.h.lastContext = res.TokensAfter
	if res.Action == compaction.ActionNone {
		if mode == compactManual {
			st.recordCompaction(ctx, trigger, res.TokensBefore, res.TokensAfter)
			return st.emit(ctx, agui.CustomEvent(agui.CustomCompaction, map[string]any{"phase": "completed", "trigger": trigger,
				"tokensBefore": res.TokensBefore, "tokensAfter": res.TokensAfter, "summarizedMessages": 0}))
		}
		return nil
	}
	previous := st.history
	st.history, st.h.summary = res.Messages, res.Summary
	if mode != compactManual {
		st.h.rehydration = st.rehydrate(ctx, previous, res.Messages)
	}
	st.h.lastPrompt = 0 // the reported size no longer matches the prompt
	st.h.compacted = true
	st.h.regrowAt = compaction.EstimateMessages(st.history) + compaction.KeepRecent(st.h.window, st.threshold())/2
	phase := "completed"
	switch res.Action {
	case compaction.ActionDropped:
		phase = "failed"
		slog.Warn("agent: summarizing old context failed; dropped it", "run_id", st.in.Run.ID, "error", res.Err)
	case compaction.ActionSummarized:
		if s := st.a.Harness.Summaries; s != nil && res.Through > 0 {
			pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			if err := s.SetThreadSummary(pctx, st.in.Run.ThreadID, store.ThreadSummary{Text: res.Summary, ThroughPosition: res.Through}); err != nil {
				slog.Error("agent: persisting the context summary failed", "run_id", st.in.Run.ID, "error", err)
			}
			cancel()
		}
	}
	if !started {
		_ = st.emit(ctx, agui.CustomEvent(agui.CustomCompaction, map[string]any{"phase": "started", "trigger": trigger, "tokensBefore": res.TokensBefore}))
	}
	if phase == "completed" {
		st.recordCompaction(ctx, trigger, res.TokensBefore, res.TokensAfter)
	}
	return st.emit(ctx, agui.CustomEvent(agui.CustomCompaction, map[string]any{"phase": phase, "trigger": trigger,
		"tokensBefore": res.TokensBefore, "tokensAfter": res.TokensAfter, "summarizedMessages": res.Folded}))
}

func (st *runState) rehydrationTokens() int {
	if st.h.rehydration == nil {
		return 0
	}
	return compaction.EstimateText(st.h.rehydration.Content)
}

// recordCompaction persists a completed compaction after the thread's last message.
func (st *runState) recordCompaction(ctx context.Context, trigger string, before, after int) {
	cs := st.a.Harness.Compactions
	if cs == nil {
		return
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := cs.RecordCompaction(pctx, store.Compaction{ThreadID: st.in.Run.ThreadID, RunID: st.in.Run.ID, Trigger: trigger,
		TokensBefore: before, TokensAfter: after}); err != nil {
		slog.Warn("agent: recording the compaction failed", "run_id", st.in.Run.ID, "error", err)
	}
}

// compactRun is a run of kind compact: it only compacts the thread's context.
func (st *runState) compactRun(ctx context.Context, base ai.ChatModel) (Result, error) {
	if err := st.compact(ctx, base, compactManual); err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, err
	}
	return Result{Usage: st.usages()}, nil
}

// usageEvent is the cumulative work.usage of the run with the context size.
func (st *runState) usageEvent() agui.Event {
	return agui.CustomEvent(agui.CustomUsage, map[string]any{
		"inputTokens": st.usage.InputTokens, "outputTokens": st.usage.OutputTokens,
		"totalTokens": st.usage.TotalTokens, "reasoningTokens": st.usage.ReasoningTokens, "cachedInputTokens": st.usage.CachedInputTokens,
		"contextTokens": st.h.lastContext, "contextWindow": st.h.window,
	})
}

// toolEmitter forwards live tool output and queues events for after the tool
// result. A buffered emitter (a call running in parallel with others) keeps
// the live output until the call's result is emitted, in call order.
type toolEmitter struct {
	st       *runState
	ctx      context.Context
	mu       sync.Mutex
	after    []agui.Event
	buffered bool
	output   []agui.Event
}

func (e *toolEmitter) ToolOutput(toolCallID, stream, text string) {
	e.EmitLive(agui.CustomEvent(agui.CustomToolOutput, map[string]any{
		"toolCallId": toolCallID, "stream": stream, "text": text,
	}))
}

// EmitLive implements tools.LiveEmitter.
func (e *toolEmitter) EmitLive(ev agui.Event) {
	if e.buffered {
		e.mu.Lock()
		e.output = append(e.output, ev)
		e.mu.Unlock()
		return
	}
	_ = e.st.emit(e.ctx, ev)
}

// flushOutput emits the live output a buffered emitter kept.
func (e *toolEmitter) flushOutput() error {
	e.mu.Lock()
	evs := e.output
	e.output = nil
	e.mu.Unlock()
	for _, ev := range evs {
		if err := e.st.emit(e.ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

// EmitAfterResult implements tools.ResultEmitter.
func (e *toolEmitter) EmitAfterResult(ev agui.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.after = append(e.after, ev)
}

func (e *toolEmitter) flushAfter() error {
	e.mu.Lock()
	evs := e.after
	e.after = nil
	e.mu.Unlock()
	for _, ev := range evs {
		if err := e.st.emit(e.ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

// interrupted handles a tool that needs the user: the first one ends the run
// after this step; later ones in the same step are refused.
func (st *runState) interrupted(tc ai.ToolCall, intr *tools.Interrupt) (string, bool) {
	if st.h.interrupt != nil {
		return OneQuestionResult, false
	}
	intr.ID = uuid.NewString()
	intr.ToolCallID = tc.ID
	st.h.interrupt = intr
	return "", true
}

func asInterrupt(err error) (*tools.Interrupt, bool) {
	var intr *tools.Interrupt
	ok := errors.As(err, &intr)
	return intr, ok
}
