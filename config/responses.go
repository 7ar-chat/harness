package config

import (
	"slices"
	"strings"
)

// Responses selects the models called through the OpenAI Responses API
// (stream R, docs/harness-v3.md "R" block): their reasoning items are carried
// across tool steps and runs. Other models stay on Chat Completions.
type Responses struct {
	// LLMResponsesModels are the LLM_MODELS ids called through the Responses API
	// ("none": every model uses Chat Completions). Only models of the gateway's
	// openai provider qualify (DeepSeek stays on Chat Completions).
	LLMResponsesModels []string `env:"LLM_RESPONSES_MODELS" envDefault:"gpt-5.6-sol,gpt-5.6-terra" envSeparator:","`
}

// UsesResponses reports whether a model is called through the Responses API.
func (c *Config) UsesResponses(model string) bool {
	if provider, _, ok := strings.Cut(model, "/"); ok && provider != "openai" {
		return false
	}
	return slices.Contains(trimAll(c.LLMResponsesModels), model)
}
