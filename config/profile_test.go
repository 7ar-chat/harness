package config_test

import (
	"testing"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/prompts"
)

func TestAgentProfileCodingSelectsTheCodingPromptUnlessVariantsAreSetExplicitly(t *testing.T) {
	// Arrange
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-5.6-sol,other")
	t.Setenv("AGENT_PROFILE", "coding")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	// Act
	defaulted, unlisted := cfg.PromptVariant("gpt-5.6-sol"), cfg.PromptVariant("other")

	// Assert
	if defaulted != prompts.Coding || unlisted != prompts.Coding {
		t.Fatalf("defaulted %q unlisted %q", defaulted, unlisted)
	}
}

func TestAgentProfileRejectsUnknownValues(t *testing.T) {
	// Arrange
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "m")
	t.Setenv("AGENT_PROFILE", "wizard")

	// Act
	_, err := config.Load()

	// Assert
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestAgentProfileCodingKeepsExplicitPromptVariants(t *testing.T) {
	// Arrange
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "a,b")
	t.Setenv("AGENT_PROFILE", "coding")
	t.Setenv("LLM_PROMPT_VARIANTS", "a=gpt-5.6")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	// Act
	a, b := cfg.PromptVariant("a"), cfg.PromptVariant("b")

	// Assert
	if a != prompts.GPT56 || b != prompts.Coding {
		t.Fatalf("a %q b %q", a, b)
	}
}
