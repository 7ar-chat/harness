package responses

import "github.com/cloudwego/eino/components/model"

// Options are the Responses-specific call options.
type Options struct {
	// ReasoningEffort is reasoning.effort (minimal|low|medium|high; "": the model's default).
	ReasoningEffort string
	// PromptCacheKey is prompt_cache_key (calls with the same key share a prompt cache).
	PromptCacheKey string
}

// WithReasoningEffort sets reasoning.effort on a call.
func WithReasoningEffort(effort string) model.Option {
	return model.WrapImplSpecificOptFn(func(o *Options) { o.ReasoningEffort = effort })
}

// WithPromptCacheKey sets prompt_cache_key on a call.
func WithPromptCacheKey(key string) model.Option {
	return model.WrapImplSpecificOptFn(func(o *Options) { o.PromptCacheKey = key })
}
