package ai

import "context"

// ChatModel answers a conversation, whole or as a stream of chunks that ConcatMessages merges.
type ChatModel interface {
	Generate(ctx context.Context, msgs []*Message, opts ...Option) (*Message, error)
	Stream(ctx context.Context, msgs []*Message, opts ...Option) (*StreamReader[*Message], error)
}

// Model is a ChatModel that can be offered tools. WithTools returns a copy; the receiver is unchanged.
type Model interface {
	ChatModel
	WithTools(tools []*ToolInfo) (Model, error)
}

// ToolChoice says whether the model may, must or must not call tools.
type ToolChoice string

// Tool choices.
const (
	ToolChoiceForbidden ToolChoice = "forbidden"
	ToolChoiceAllowed   ToolChoice = "allowed"
	ToolChoiceForced    ToolChoice = "forced"
)

// CallOptions are the settings of one model call.
type CallOptions struct {
	Temperature *float32
	TopP        *float32
	MaxTokens   *int
	// ToolChoice constrains tool use; AllowedToolNames narrows a forced choice to those tools.
	ToolChoice       *ToolChoice
	AllowedToolNames []string
	// ReasoningEffort is the reasoning effort requested of a reasoning model ("": its default).
	ReasoningEffort string
	// PromptCacheKey groups calls that share a prompt cache.
	PromptCacheKey string
}

// Option changes the CallOptions of one call.
type Option func(*CallOptions)

// Apply returns the options o configure.
func Apply(opts ...Option) CallOptions {
	var c CallOptions
	for _, o := range opts {
		if o != nil {
			o(&c)
		}
	}
	return c
}

// WithToolChoice sets how the model may use its tools.
func WithToolChoice(choice ToolChoice, allowedToolNames ...string) Option {
	return func(c *CallOptions) { c.ToolChoice, c.AllowedToolNames = &choice, allowedToolNames }
}

// WithTemperature sets the sampling temperature.
func WithTemperature(t float32) Option { return func(c *CallOptions) { c.Temperature = &t } }

// WithTopP sets nucleus sampling.
func WithTopP(p float32) Option { return func(c *CallOptions) { c.TopP = &p } }

// WithMaxTokens caps the completion.
func WithMaxTokens(n int) Option { return func(c *CallOptions) { c.MaxTokens = &n } }

// WithReasoningEffort sets the reasoning effort of a reasoning model.
func WithReasoningEffort(effort string) Option {
	return func(c *CallOptions) { c.ReasoningEffort = effort }
}

// WithPromptCacheKey makes calls with the same key share a prompt cache.
func WithPromptCacheKey(key string) Option { return func(c *CallOptions) { c.PromptCacheKey = key } }
