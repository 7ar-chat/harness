package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/agui"
	"github.com/GhaziBenDahmane/harness/llm"
)

// Harness v3 §1: model call budgets, retries and the texts of their outcomes.
const (
	defaultModelTimeout = 300 * time.Second
	defaultStreamIdle   = 90 * time.Second
	defaultCallCeiling  = 20 * time.Minute
	defaultStepRetries  = 3

	// StreamRetriedResult is the TOOL_CALL_RESULT of a call announced by an attempt that is retried.
	StreamRetriedResult = "(stream interrupted, retrying)"
	// StreamFailedResult is the TOOL_CALL_RESULT of a call announced by the last, failed attempt.
	StreamFailedResult = "(stream interrupted)"

	// Messages of the v3 RUN_ERROR codes (user-facing, never provider details).
	MsgModelTimeout  = "The AI model did not respond in time. Please try again."
	MsgRateLimited   = "The AI model is overloaded right now (rate limited). Please try again in a few minutes."
	MsgContentFilter = "The AI provider's content filter stopped the answer. Rephrase the request and try again."
	MsgEmptyAnswer   = "The AI model returned an empty answer. Please try again or rephrase the request."
)

// errStalled is the cause of an attempt cut by the watchdog.
var errStalled = errors.New("the model stream was silent for too long")

// stepOutput is the outcome of a successful model call.
type stepOutput struct {
	msg         *schema.Message
	assistantID string
	reasoningID string
	reasoning   string
	// refusal is the provider's refusal text (delta.refusal), if any.
	refusal string
	// announced are the tool calls the stream announced (TOOL_CALL_START), in order.
	announced []string
}

func (o *stepOutput) finishReason() string {
	if o == nil || o.msg == nil || o.msg.ResponseMeta == nil {
		return ""
	}
	return o.msg.ResponseMeta.FinishReason
}

func (a *Agent) modelTimeout() time.Duration {
	if a.ModelTimeout > 0 {
		return a.ModelTimeout
	}
	return defaultModelTimeout
}

func (a *Agent) streamIdle() time.Duration {
	if a.StreamIdleTimeout > 0 {
		return a.StreamIdleTimeout
	}
	return defaultStreamIdle
}

// StepRetriesSetting maps LLM_STEP_RETRIES to Agent.StepRetries (0 retries: -1).
func StepRetriesSetting(n int) int {
	if n <= 0 {
		return -1
	}
	return n
}

func (a *Agent) stepRetries() int {
	switch {
	case a.StepRetries < 0:
		return 0
	case a.StepRetries == 0:
		return defaultStepRetries
	}
	return a.StepRetries
}

func (a *Agent) sleep(ctx context.Context, d time.Duration) error {
	if a.Sleep != nil {
		return a.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// attemptResult is the outcome of one attempt of a model call.
type attemptResult struct {
	out *stepOutput
	t   *translator
	err error
	// class sorts err (timeout when the watchdog cut the stream).
	class      llm.ErrorClass
	retryAfter time.Duration
	// fatal is an event-log failure: the run ends without retry.
	fatal error
}

// callModel is the model call of a step (harness v3 §1). A call that fails
// before or during its stream with a retryable error (timeout, 429, 5xx,
// connection reset) is re-issued with the same request, up to StepRetries
// times: the failed attempt's events are closed (its tool calls get a
// "retried" error result), nothing of it is persisted, and CUSTOM work.retry
// precedes the new attempt. A cancelled call returns its partial output with
// ctx.Err().
func (st *runState) callModel(ctx context.Context, m model.ToolCallingChatModel, msgs []*schema.Message, opts ...model.Option) (*stepOutput, error) {
	retries := st.a.stepRetries()
	for attempt := 1; ; attempt++ {
		res := st.attempt(ctx, m, msgs, opts...)
		switch {
		case res.fatal != nil:
			return nil, res.fatal
		case res.err == nil:
			return res.out, nil
		case ctx.Err() != nil:
			return res.out, ctx.Err()
		}
		retry := res.class.Retryable() && attempt <= retries
		if res.t != nil {
			if err := res.t.abort(ctx, retry); err != nil {
				return nil, err
			}
		}
		if !retry {
			return nil, modelFailure(res)
		}
		slog.Warn("agent: model call failed, retrying the step", "run_id", st.in.Run.ID, "attempt", attempt, "class", res.class, "error", res.err)
		if err := st.emit(ctx, agui.CustomEvent(agui.CustomRetry, map[string]any{"attempt": attempt + 1, "reason": string(res.class)})); err != nil {
			return nil, err
		}
		if err := st.a.sleep(ctx, llm.Backoff(attempt, st.a.RetryBackoff, res.retryAfter, rand.Float64())); err != nil {
			return nil, err
		}
	}
}

// modelFailure is the run error of a model call that will not be retried.
func modelFailure(res attemptResult) *Error {
	detail := truncate("model call: "+res.err.Error(), 1000)
	switch res.class {
	case llm.ClassTimeout:
		return &Error{Code: agui.CodeModelTimeout, Message: MsgModelTimeout, Detail: detail, cause: res.err}
	case llm.ClassRateLimited:
		return &Error{Code: agui.CodeRateLimited, Message: MsgRateLimited, Detail: detail, cause: res.err}
	}
	return &Error{Code: agui.CodeModelError, Message: MsgModelError, Detail: detail, cause: res.err}
}

// watchdog cancels an attempt when the first chunk takes longer than first, a
// later chunk longer than idle after the previous one, or the whole call
// longer than its ceiling. A provider event the client does not forward
// restarts the current budget (a model reasoning silently is not stalled);
// SSE comment lines (keep-alives) do not (round 3, N3).
type watchdog struct {
	mu      sync.Mutex
	timer   *time.Timer
	ceiling *time.Timer
	first   time.Duration
	idle    time.Duration
	started bool
	fired   atomic.Bool
}

func startWatchdog(cancel context.CancelFunc, first, idle, ceiling time.Duration) *watchdog {
	w := &watchdog{first: first, idle: idle}
	fire := func() {
		w.fired.Store(true)
		cancel()
	}
	w.timer = time.AfterFunc(first, fire)
	w.ceiling = time.AfterFunc(ceiling, fire) // whatever the stream sends (round 3, N3)
	return w
}

// kick: a chunk arrived; the next one must follow within idle.
func (w *watchdog) kick() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.started = true
	if !w.fired.Load() {
		w.timer.Reset(w.idle)
	}
}

// activity: the stream is alive; its current budget starts again.
func (w *watchdog) activity() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fired.Load() {
		return
	}
	if w.started {
		w.timer.Reset(w.idle)
	} else {
		w.timer.Reset(w.first)
	}
}

func (w *watchdog) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.timer.Stop()
	w.ceiling.Stop()
}

// firstChunkBudget is LLM_TIMEOUT_SECONDS, ×2 at high and ×3 at xhigh
// reasoning effort (a long silent reasoning before the first chunk).
func (st *runState) firstChunkBudget() time.Duration {
	return st.effortScaled(st.a.modelTimeout())
}

// callCeiling is LLM_CALL_MAX_SECONDS (default 20 min), scaled like the first-chunk budget.
func (st *runState) callCeiling() time.Duration {
	d := st.a.CallCeiling
	if d <= 0 {
		d = defaultCallCeiling
	}
	return st.effortScaled(d)
}

func (st *runState) effortScaled(d time.Duration) time.Duration {
	if f := st.a.Harness.SupportsReasoning; f == nil || !f(st.in.Run.Model) {
		return d
	}
	switch strings.ToLower(st.in.Run.ReasoningEffort) {
	case "high":
		return 2 * d
	case "xhigh":
		return 3 * d
	}
	return d
}

// attempt is one model call: it streams the answer as AG-UI events.
func (st *runState) attempt(ctx context.Context, m model.ToolCallingChatModel, msgs []*schema.Message, opts ...model.Option) attemptResult {
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	actx, info := llm.WithCallInfo(actx)
	wd := startWatchdog(cancel, st.firstChunkBudget(), st.a.streamIdle(), st.callCeiling())
	defer wd.stop()
	info.OnActivity(wd.activity)
	fail := func(t *translator, err error) attemptResult {
		class := llm.Classify(err)
		if wd.fired.Load() && ctx.Err() == nil {
			class, err = llm.ClassTimeout, fmt.Errorf("%w: %v", errStalled, err)
		}
		return attemptResult{t: t, err: err, class: class, retryAfter: info.RetryAfter()}
	}

	sr, err := m.Stream(actx, msgs, opts...)
	if err != nil {
		return fail(nil, err)
	}
	defer sr.Close()
	t := &translator{st: st, assistantID: uuid.NewString(), calls: map[string]*liveCall{}, byIndex: map[int]string{}}
	var chunks []*schema.Message
	var streamErr error
	for {
		chunk, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			streamErr = err
			break
		}
		wd.kick()
		chunks = append(chunks, chunk)
		if err := t.handle(ctx, chunk); err != nil {
			return attemptResult{fatal: err}
		}
	}
	wd.stop()
	if streamErr != nil && ctx.Err() == nil {
		return fail(t, streamErr)
	}

	// Tool calls are merged by the translator: concatenating them would fail on
	// providers that reuse an Index for parallel calls with distinct ids (A-18).
	final := &schema.Message{Role: schema.Assistant}
	if len(chunks) > 0 {
		stripped := make([]*schema.Message, 0, len(chunks))
		for _, c := range chunks {
			cp := *c
			cp.ToolCalls = nil
			stripped = append(stripped, &cp)
		}
		if merged, err := schema.ConcatMessages(stripped); err == nil {
			final = merged
		} else {
			slog.Warn("agent: merging stream chunks failed", "run_id", st.in.Run.ID, "error", err)
			var text strings.Builder
			for _, c := range chunks {
				text.WriteString(c.Content)
			}
			final.Content = text.String()
		}
	}
	calls, err := t.finish(ctx, streamErr != nil)
	if err != nil {
		return attemptResult{fatal: err}
	}
	final.ToolCalls = calls
	out := &stepOutput{msg: final, assistantID: t.assistantID, reasoningID: t.reasoningID, reasoning: t.reasoning.String(),
		refusal: info.Refusal(), announced: t.announced()}
	if streamErr != nil { // cancelled: the partial answer is kept
		return attemptResult{out: out, err: ctx.Err()}
	}
	return attemptResult{out: out}
}

type liveCall struct {
	id, name string
	started  bool
	ended    bool
	pending  strings.Builder // arguments not yet emitted
	args     strings.Builder // all arguments (the merged call)
}

// translator turns stream chunks into AG-UI events in strict order: the text
// message ends before the first tool call starts, and each tool call ends
// before the next one starts (A-19).
type translator struct {
	st            *runState
	assistantID   string
	textOpen      bool
	reasoningID   string
	reasoningOpen bool
	reasoning     strings.Builder
	calls         map[string]*liveCall
	order         []*liveCall
	byIndex       map[int]string
	open          *liveCall // the started call that is not ended yet
	lastKey       string
	chunks        int
}

// callKey identifies the call a tool-call fragment belongs to: Index (a new
// call when the fragment brings another id at the same Index), else ID.
// Without both, only the first fragment of a chunk may continue the previous
// call (argument streaming), and only when it does not name a new tool;
// other positions are new calls.
func (t *translator) callKey(tc schema.ToolCall, pos int) string {
	switch {
	case tc.Index != nil:
		key, ok := t.byIndex[*tc.Index]
		if !ok {
			key = fmt.Sprintf("i:%d", *tc.Index)
		}
		if c := t.calls[key]; c != nil && tc.ID != "" && c.id != "" && tc.ID != c.id {
			key = fmt.Sprintf("i:%d:%s", *tc.Index, tc.ID)
		}
		t.byIndex[*tc.Index] = key
		return key
	case tc.ID != "":
		return "id:" + tc.ID
	}
	if pos == 0 && t.lastKey != "" {
		prev := t.calls[t.lastKey]
		if tc.Function.Name == "" || prev == nil || prev.name == "" {
			return t.lastKey
		}
	}
	return fmt.Sprintf("p:%d:%d", t.chunks, pos)
}

func (t *translator) closeReasoning(ctx context.Context) error {
	if !t.reasoningOpen {
		return nil
	}
	t.reasoningOpen = false
	if err := t.st.emit(ctx, agui.New(agui.ReasoningMessageEnd, map[string]any{"messageId": t.reasoningID})); err != nil {
		return err
	}
	return t.st.emit(ctx, agui.New(agui.ReasoningEnd, map[string]any{"messageId": t.reasoningID}))
}

func (t *translator) closeText(ctx context.Context) error {
	if !t.textOpen {
		return nil
	}
	t.textOpen = false
	return t.st.emit(ctx, agui.New(agui.TextMessageEnd, map[string]any{"messageId": t.assistantID}))
}

func (t *translator) endOpenCall(ctx context.Context) error {
	c := t.open
	if c == nil {
		return nil
	}
	t.open = nil
	c.ended = true
	return t.st.emit(ctx, agui.New(agui.ToolCallEnd, map[string]any{"toolCallId": c.id}))
}

func (t *translator) startCall(ctx context.Context, c *liveCall) error {
	if err := t.closeReasoning(ctx); err != nil {
		return err
	}
	if err := t.closeText(ctx); err != nil {
		return err
	}
	if err := t.endOpenCall(ctx); err != nil {
		return err
	}
	c.started = true
	t.open = c
	if err := t.st.emit(ctx, agui.New(agui.ToolCallStart, map[string]any{
		"toolCallId": c.id, "toolCallName": c.name, "parentMessageId": t.assistantID,
	})); err != nil {
		return err
	}
	if c.pending.Len() > 0 {
		args := c.pending.String()
		c.pending.Reset()
		return t.st.emit(ctx, agui.New(agui.ToolCallArgs, map[string]any{"toolCallId": c.id, "delta": args}))
	}
	return nil
}

func (t *translator) handle(ctx context.Context, chunk *schema.Message) error {
	if chunk == nil {
		return nil
	}
	if chunk.ReasoningContent != "" {
		if !t.reasoningOpen {
			if err := t.closeText(ctx); err != nil {
				return err
			}
			if err := t.endOpenCall(ctx); err != nil {
				return err
			}
			if t.reasoningID == "" {
				t.reasoningID = uuid.NewString()
			}
			t.reasoningOpen = true
			if err := t.st.emit(ctx, agui.New(agui.ReasoningStart, map[string]any{"messageId": t.reasoningID})); err != nil {
				return err
			}
			if err := t.st.emit(ctx, agui.New(agui.ReasoningMessageStart, map[string]any{"messageId": t.reasoningID, "role": "reasoning"})); err != nil {
				return err
			}
		}
		t.reasoning.WriteString(chunk.ReasoningContent)
		if err := t.st.emit(ctx, agui.New(agui.ReasoningMessageContent, map[string]any{"messageId": t.reasoningID, "delta": chunk.ReasoningContent})); err != nil {
			return err
		}
	}
	if chunk.Content != "" {
		if err := t.closeReasoning(ctx); err != nil {
			return err
		}
		if err := t.endOpenCall(ctx); err != nil {
			return err
		}
		if !t.textOpen {
			// Text after a tool call (rare) reopens the assistant message.
			t.textOpen = true
			if err := t.st.emit(ctx, agui.New(agui.TextMessageStart, map[string]any{"messageId": t.assistantID, "role": "assistant"})); err != nil {
				return err
			}
		}
		if err := t.st.emit(ctx, agui.New(agui.TextMessageContent, map[string]any{"messageId": t.assistantID, "delta": chunk.Content})); err != nil {
			return err
		}
	}
	t.chunks++
	for i, tc := range chunk.ToolCalls {
		key := t.callKey(tc, i)
		t.lastKey = key
		c := t.calls[key]
		if c == nil {
			c = &liveCall{}
			t.calls[key] = c
			t.order = append(t.order, c)
		}
		if c.id == "" && tc.ID != "" {
			c.id = tc.ID
		}
		if c.name == "" && tc.Function.Name != "" {
			c.name = tc.Function.Name
		}
		c.args.WriteString(tc.Function.Arguments)
		if !c.started {
			c.pending.WriteString(tc.Function.Arguments)
			if c.id != "" && c.name != "" {
				if err := t.startCall(ctx, c); err != nil {
					return err
				}
			}
			continue
		}
		if tc.Function.Arguments != "" && !c.ended {
			if err := t.st.emit(ctx, agui.New(agui.ToolCallArgs, map[string]any{"toolCallId": c.id, "delta": tc.Function.Arguments})); err != nil {
				return err
			}
		}
	}
	return nil
}

// announced lists the ids of the calls that got a TOOL_CALL_START.
func (t *translator) announced() []string {
	var ids []string
	for _, c := range t.order {
		if c.started {
			ids = append(ids, c.id)
		}
	}
	return ids
}

// finish closes open messages and returns the tool calls merged by the
// translator itself (schema.ConcatMessages does not merge Index-less fragments).
// When the stream broke (cancelled), announced calls are ended and no call is returned.
func (t *translator) finish(ctx context.Context, broken bool) ([]schema.ToolCall, error) {
	if err := t.closeReasoning(ctx); err != nil {
		return nil, err
	}
	if err := t.closeText(ctx); err != nil {
		return nil, err
	}
	var calls []schema.ToolCall
	for _, c := range t.order {
		if broken && !c.started {
			continue
		}
		if !c.started {
			if c.id == "" {
				c.id = "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			}
			if err := t.startCall(ctx, c); err != nil {
				return nil, err
			}
		}
		if !broken {
			calls = append(calls, schema.ToolCall{ID: c.id, Type: "function", Function: schema.FunctionCall{Name: c.name, Arguments: c.args.String()}})
		}
	}
	if err := t.endOpenCall(ctx); err != nil {
		return nil, err
	}
	return calls, nil
}

// abort closes the events of a failed attempt: reasoning and text end, every
// announced tool call ends and gets an error result (retried: another attempt follows).
func (t *translator) abort(ctx context.Context, retried bool) error {
	if err := t.closeReasoning(ctx); err != nil {
		return err
	}
	if err := t.closeText(ctx); err != nil {
		return err
	}
	if err := t.endOpenCall(ctx); err != nil {
		return err
	}
	content, md := StreamFailedResult, map[string]any{"isError": true}
	if retried {
		content, md = StreamRetriedResult, map[string]any{"isError": true, "retried": true}
	}
	for _, c := range t.order {
		if !c.started {
			continue
		}
		if err := t.st.emit(ctx, agui.New(agui.ToolCallResult, map[string]any{
			"messageId": uuid.NewString(), "toolCallId": c.id, "content": content, "role": "tool", "metadata": md,
		})); err != nil {
			return err
		}
	}
	return nil
}
