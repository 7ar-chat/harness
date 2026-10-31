package main

import (
	"context"
	"net/http"

	"github.com/GhaziBenDahmane/harness/ai/llm"
	"github.com/GhaziBenDahmane/harness/config"
)

// discoverer returns the func /model uses to ask the selected provider which
// models it serves. The key goes only to the address the configuration chose
// (it was already trusted to receive it for the runs themselves).
func discoverer(cfg *config.Config) func(context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		return llm.Discover(ctx, http.DefaultClient, llm.Options{Provider: cfg.LLMProvider, BaseURL: cfg.LLMBaseURL, APIKey: cfg.LLMAPIKey})
	}
}
