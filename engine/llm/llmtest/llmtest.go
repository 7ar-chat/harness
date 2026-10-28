// Package llmtest provides a scripted fake chat model for tests.
package llmtest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/engine/llm"
)

// Turn is one scripted model response.
type Turn struct {
	// Chunks are streamed in order.
	Chunks []*schema.Message
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
	Messages []*schema.Message
	Tools    []*schema.ToolInfo
	// ToolChoice is the model.WithToolChoice option, if any.
	ToolChoice *schema.ToolChoice
	// ReasoningEffort is the llm.WithReasoningEffort option, if any.
	ReasoningEffort string
	// PromptCacheKey is the llm.WithPromptCacheKey option, if any.
	PromptCacheKey string
}

// Model is a scripted model.ToolCallingChatModel.
type Model struct {
	mu    sync.Mutex
	turns []Turn
	calls []Call
	tools []*schema.ToolInfo
	root  *Model
	fn    func(Call) Turn
	// Responses backend (responses.go): the request bodies and the clients by model.
	requests []map[string]any
	clients  map[string]model.ToolCallingChatModel
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

// WithTools implements model.ToolCallingChatModel.
func (m *Model) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return &Model{root: m.r(), tools: tools}, nil
}

func (m *Model) next(ctx context.Context, msgs []*schema.Message, opts []model.Option) Turn {
	r := m.r()
	r.mu.Lock()
	defer r.mu.Unlock()
	common := model.GetCommonOptions(nil, opts...)
	impl := model.GetImplSpecificOptions(&llm.CallOptions{}, opts...)
	call := Call{Messages: append([]*schema.Message(nil), msgs...), Tools: m.tools, ToolChoice: common.ToolChoice, ReasoningEffort: impl.ReasoningEffort,
		PromptCacheKey: impl.PromptCacheKey}
	r.calls = append(r.calls, call)
	if r.fn != nil {
		return r.fn(call)
	}
	if len(r.turns) == 0 {
		return Turn{Chunks: []*schema.Message{schema.AssistantMessage("done", nil)}}
	}
	t := r.turns[0]
	r.turns = r.turns[1:]
	return t
}

// Stream implements model.BaseChatModel.
func (m *Model) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	t := m.next(ctx, msgs, opts)
	if t.OnCall != nil {
		t.OnCall()
	}
	if t.Err != nil {
		llm.RecordRetryAfter(ctx, t.RetryAfter)
		return nil, t.Err
	}
	llm.RecordRefusal(ctx, t.Refusal)
	sr, sw := schema.Pipe[*schema.Message](len(t.Chunks) + 1)
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

// Generate implements model.BaseChatModel.
func (m *Model) Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	sr, err := m.Stream(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	defer sr.Close()
	var chunks []*schema.Message
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
	return schema.ConcatMessages(chunks)
}

// Factory always returns the same model: the scripted model itself, or with
// Responses (or UseResponses) the real Responses client playing its turns
// through the fake Responses server (ids with a "provider/" prefix excepted).
type Factory struct {
	M         *Model
	Responses bool
}

// Model implements llm.Factory.
func (f Factory) Model(ctx context.Context, name string) (model.ToolCallingChatModel, error) {
	if (f.Responses || UsingResponses()) && !strings.Contains(name, "/") {
		return f.M.responsesClient(ctx, name)
	}
	return f.M, nil
}

// Text builds streamed text chunks.
func Text(parts ...string) []*schema.Message {
	out := make([]*schema.Message, 0, len(parts))
	for _, p := range parts {
		out = append(out, &schema.Message{Role: schema.Assistant, Content: p})
	}
	return out
}

// ToolCall builds a single-chunk tool call at index idx.
func ToolCall(idx int, id, name, args string) *schema.Message {
	i := idx
	return &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
		Index: &i, ID: id, Type: "function", Function: schema.FunctionCall{Name: name, Arguments: args},
	}}}
}

// Finish sets the provider's finish_reason (stop, length, tool_calls, content_filter) on a chunk.
func Finish(m *schema.Message, reason string) *schema.Message {
	if m.ResponseMeta == nil {
		m.ResponseMeta = &schema.ResponseMeta{}
	}
	m.ResponseMeta.FinishReason = reason
	return m
}

// WithCachedUsage sets usage with cached prompt tokens on a chunk.
func WithCachedUsage(m *schema.Message, prompt, cached, completion int) *schema.Message {
	WithUsage(m, prompt, completion)
	m.ResponseMeta.Usage.PromptTokenDetails.CachedTokens = cached
	return m
}

// Reasoning builds a streamed reasoning chunk.
func Reasoning(text string) *schema.Message {
	return &schema.Message{Role: schema.Assistant, ReasoningContent: text}
}

// WithUsage sets usage on a chunk.
func WithUsage(m *schema.Message, prompt, completion int) *schema.Message {
	if m.ResponseMeta == nil {
		m.ResponseMeta = &schema.ResponseMeta{}
	}
	m.ResponseMeta = &schema.ResponseMeta{FinishReason: m.ResponseMeta.FinishReason, Usage: &schema.TokenUsage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion}}
	return m
}
