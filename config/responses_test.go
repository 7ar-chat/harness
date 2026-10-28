package config_test

import (
	"testing"

	"github.com/GhaziBenDahmane/harness/config"
)

func TestLoadCallsGPTModelsThroughTheResponsesAPI(t *testing.T) {
	// By default gpt-5.6-sol and gpt-5.6-terra use the Responses API; DeepSeek and unlisted models keep Chat Completions.
	// Arrange
	t.Setenv("LLM_MODELS", "gpt-5.6-sol,gpt-5.6-terra,gpt-4.1,deepseek/DeepSeek-V4-Flash")

	// Act
	cfg, err := config.Load()

	// Assert
	if err != nil || !cfg.UsesResponses("gpt-5.6-sol") || !cfg.UsesResponses("gpt-5.6-terra") || cfg.UsesResponses("gpt-4.1") ||
		cfg.UsesResponses("deepseek/DeepSeek-V4-Flash") {
		t.Fatalf("err %v responses models %v", err, cfg.LLMResponsesModels)
	}
}

func TestLoadResponsesModelsOverride(t *testing.T) {
	// LLM_RESPONSES_MODELS replaces the list; "none" keeps every model on Chat Completions; a DeepSeek id never qualifies.
	// Arrange
	t.Setenv("LLM_RESPONSES_MODELS", "gpt-4.1")
	listed, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLM_RESPONSES_MODELS", "none")

	// Act
	none, err := config.Load()

	// Assert
	if err != nil || !listed.UsesResponses("gpt-4.1") || listed.UsesResponses("gpt-5.6-terra") ||
		none.UsesResponses("gpt-5.6-terra") {
		t.Fatalf("err %v listed %v none %v", err, listed.LLMResponsesModels, none.LLMResponsesModels)
	}
}
