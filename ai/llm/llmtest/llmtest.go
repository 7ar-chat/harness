// Package llmtest provides a scripted fake chat model for tests of code that talks to an ai.Model.
//
// A Model plays a list of Turns, one per call, streaming each turn's chunks the way a provider would, and records
// every call (messages, tools, options) for assertions. A Turn can also fail, stall, send keep-alives, report
// a refusal or a Retry-After hint, so the retry and watchdog paths of a caller can be exercised without a network.
package llmtest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/llm"
)

// Turn is one scripted model response.
type Turn struct {
	// Chunks are streamed in order.
	Chunks []*ai.Message
	// Err is returned by Stream before any chunk.
	Err error
	// FailAfter, when set, makes the stream fail with this error after Chunks.
	FailAfter error
	// Block makes the stream wait for ctx cancellation after Chunks.
	Block bool
	// OnCall runs when the turn is requested (before streaming).
	OnCall func()
	// RetryAfter is the provider's Retry-After hint recorded with Err (a 429/5xx).
	RetryAfter time.Duration
	// Refusal is streamed as delta.refusal (recorded like the real transport does).
	Refusal string
	// ChunkDelay is waited before each chunk (a slow but live stream).
	ChunkDelay time.Duration
	// KeepAlive is a silence before the first chunk during which the
	// provider only sends progress events the client does not forward (every
	// 10 ms): a live stream that forwards no chunk (silent reasoning).
	KeepAlive time.Duration
	// Pings is a silence before the first chunk during which only SSE comment
	// lines (": keep-alive") arrive, every 10 ms: a gateway pinging while the
	// upstream hangs. They are no provider activity (the scripted Chat model
	// sends nothing at all).
	Pings time.Duration
}

// keepAliveEvery is the period of a turn's keep-alives.
const keepAliveEvery = 10 * time.Millisecond

// keepAlive waits d, calling ping every keepAliveEvery; false when ctx ended.
func keepAlive(ctx context.Context, d time.Duration, ping func()) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		ping()
		select {
		case <-ctx.Done():
			return false
		case <-time.After(keepAliveEvery):
		}
	}
	return true
}

// Call records one model invocation.
type Call struct {
	Messages []*ai.Message
	Tools    []*ai.ToolInfo
	// ToolChoice is the ai.WithToolChoice option, if any.
	ToolChoice *ai.ToolChoice
	// ReasoningEffort is the ai.WithReasoningEffort option, if any.
	ReasoningEffort string
	// PromptCacheKey is the ai.WithPromptCacheKey option, if any.
	PromptCacheKey string
}

// Model is a scripted ai.Model.
type Model struct {
	mu    sync.Mutex
	turns []Turn
	calls []Call
	tools []*ai.ToolInfo
	root  *Model
	fn    func(Call) Turn
	// Responses backend (responses.go): the request bodies and the clients by model.
	requests []map[string]any
	clients  map[string]ai.Model
}

// New returns a model that plays the turns in order (then answers "done").
func New(turns ...Turn) *Model { return &Model{turns: turns} }

// NewFunc returns a model whose turns are computed from each call (calls are recorded as usual).
func NewFunc(fn func(Call) Turn) *Model { return &Model{fn: fn} }

func (m *Model) r() *Model {
	if m.root != nil {
		return m.root
	}
	return m
}

// Calls returns the recorded invocations.
func (m *Model) Calls() []Call {
	r := m.r()
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.calls...)
}

// WithTools implements ai.Model.
func (m *Model) WithTools(tools []*ai.ToolInfo) (ai.Model, error) {
	return &Model{root: m.r(), tools: tools}, nil
}

func (m *Model) next(ctx context.Context, msgs []*ai.Message, opts []ai.Option) Turn {
	r := m.r()
	r.mu.Lock()
	defer r.mu.Unlock()
	common := ai.Apply(opts...)
	call := Call{Messages: append([]*ai.Message(nil), msgs...), Tools: m.tools, ToolChoice: common.ToolChoice, ReasoningEffort: common.ReasoningEffort,
		PromptCacheKey: common.PromptCacheKey}
	r.calls = append(r.calls, call)
	if r.fn != nil {
		return r.fn(call)
	}
	if len(r.turns) == 0 {
		return Turn{Chunks: []*ai.Message{ai.AssistantMessage("done", nil)}}
	}
	t := r.turns[0]
	r.turns = r.turns[1:]
	return t
}

// Stream implements ai.ChatModel.
func (m *Model) Stream(ctx context.Context, msgs []*ai.Message, opts ...ai.Option) (*ai.StreamReader[*ai.Message], error) {
	t := m.next(ctx, msgs, opts)
	if t.OnCall != nil {
		t.OnCall()
	}
	if t.Err != nil {
		llm.RecordRetryAfter(ctx, t.RetryAfter)
		return nil, t.Err
	}
	llm.RecordRefusal(ctx, t.Refusal)
	sr, sw := ai.Pipe[*ai.Message](len(t.Chunks) + 1)
	go func() {
		defer sw.Close()
		if t.KeepAlive > 0 && !keepAlive(ctx, t.KeepAlive, func() { llm.RecordActivity(ctx) }) {
			sw.Send(nil, ctx.Err())
			return
		}
		if t.Pings > 0 && !keepAlive(ctx, t.Pings, func() {}) {
			sw.Send(nil, ctx.Err())
			return
		}
		for _, c := range t.Chunks {
			if t.ChunkDelay > 0 {
				select {
				case <-ctx.Done():
					sw.Send(nil, ctx.Err())
					return
				case <-time.After(t.ChunkDelay):
				}
			}
			if sw.Send(c, nil) {
				return
			}
		}
		if t.Block {
			<-ctx.Done()
			sw.Send(nil, ctx.Err())
			return
		}
		if t.FailAfter != nil {
			sw.Send(nil, t.FailAfter)
		}
	}()
	return sr, nil
}

// Generate implements ai.ChatModel.
func (m *Model) Generate(ctx context.Context, msgs []*ai.Message, opts ...ai.Option) (*ai.Message, error) {
	sr, err := m.Stream(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	defer sr.Close()
	var chunks []*ai.Message
	for {
		c, err := sr.Recv()
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil, err
			}
			break
		}
		chunks = append(chunks, c)
	}
	return ai.ConcatMessages(chunks)
}

// Factory always returns the same model: the scripted model itself, or with
// Responses (or UseResponses) the real Responses client playing its turns
// through the fake Responses server (ids with a "provider/" prefix excepted).
type Factory struct {
	M         *Model
	Responses bool
}

// Model implements llm.Factory.
func (f Factory) Model(ctx context.Context, name string) (ai.Model, error) {
	if (f.Responses || UsingResponses()) && !strings.Contains(name, "/") {
		return f.M.responsesClient(ctx, name)
	}
	return f.M, nil
}

// Text builds streamed text chunks.
func Text(parts ...string) []*ai.Message {
	out := make([]*ai.Message, 0, len(parts))
	for _, p := range parts {
		out = append(out, &ai.Message{Role: ai.RoleAssistant, Content: p})
	}
	return out
}

// ToolCall builds a single-chunk tool call at index idx.
func ToolCall(idx int, id, name, args string) *ai.Message {
	i := idx
	return &ai.Message{Role: ai.RoleAssistant, ToolCalls: []ai.ToolCall{{
		Index: &i, ID: id, Type: "function", Function: ai.FunctionCall{Name: name, Arguments: args},
	}}}
}

// Finish sets the provider's finish_reason (stop, length, tool_calls, content_filter) on a chunk.
func Finish(m *ai.Message, reason string) *ai.Message {
	if m.ResponseMeta == nil {
		m.ResponseMeta = &ai.ResponseMeta{}
	}
	m.ResponseMeta.FinishReason = reason
	return m
}

// WithCachedUsage sets usage with cached prompt tokens on a chunk.
func WithCachedUsage(m *ai.Message, prompt, cached, completion int) *ai.Message {
	WithUsage(m, prompt, completion)
	m.ResponseMeta.Usage.PromptTokenDetails.CachedTokens = cached
	return m
}

// Reasoning builds a streamed reasoning chunk.
func Reasoning(text string) *ai.Message {
	return &ai.Message{Role: ai.RoleAssistant, ReasoningContent: text}
}

// WithUsage sets usage on a chunk.
func WithUsage(m *ai.Message, prompt, completion int) *ai.Message {
	if m.ResponseMeta == nil {
		m.ResponseMeta = &ai.ResponseMeta{}
	}
	m.ResponseMeta = &ai.ResponseMeta{FinishReason: m.ResponseMeta.FinishReason, Usage: &ai.TokenUsage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion}}
	return m
}
