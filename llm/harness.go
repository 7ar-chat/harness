package llm

import (
	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"

	"github.com/GhaziBenDahmane/harness/llm/responses"
)

// CallOptions is Work's own view of per-call options (read by the test model).
type CallOptions struct {
	ReasoningEffort string
	PromptCacheKey  string
}

// WithReasoningEffort sets reasoning_effort (minimal|low|medium|high) on a call
// (reasoning.effort on the Responses API).
func WithReasoningEffort(effort string) []model.Option {
	return []model.Option{
		einoopenai.WithReasoningEffort(einoopenai.ReasoningEffortLevel(effort)),
		responses.WithReasoningEffort(effort),
		model.WrapImplSpecificOptFn(func(o *CallOptions) { o.ReasoningEffort = effort }),
	}
}

// WithPromptCacheKey sends prompt_cache_key (OpenAI prompt caching: calls with
// the same key are routed to the same cache). Only the gateway's openai
// provider accepts it; DeepSeek rejects unknown body fields (spike, 2.7).
func WithPromptCacheKey(key string) []model.Option {
	return []model.Option{
		einoopenai.WithExtraFields(map[string]any{"prompt_cache_key": key}),
		responses.WithPromptCacheKey(key),
		model.WrapImplSpecificOptFn(func(o *CallOptions) { o.PromptCacheKey = key }),
	}
}
