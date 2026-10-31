package config_test

import (
	"strings"
	"testing"

	"github.com/7ar-chat/harness/config"
)

func TestLoadResolvesPromptVariantsPerModel(t *testing.T) {
	// GPT-5.6 models use the gpt-5.6 prompt by default, DeepSeek models the deepseek one, others the default; LLM_PROMPT_VARIANTS overrides and is validated.
	// Arrange
	t.Setenv("LLM_MODELS", "gpt-5.6-sol,gpt-5.6-terra,deepseek/DeepSeek-V4-Flash,DeepSeek-V4-Pro,mistral-large")

	// Act
	cfg, err := config.Load()

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]string{"gpt-5.6-sol": "gpt-5.6", "gpt-5.6-terra": "gpt-5.6", "deepseek/DeepSeek-V4-Flash": "deepseek", "DeepSeek-V4-Pro": "deepseek", "mistral-large": "default"} {
		if got := cfg.PromptVariant(model); got != want {
			t.Errorf("%s: variant %q, want %q", model, got, want)
		}
	}
	t.Setenv("LLM_PROMPT_VARIANTS", "gpt-5.6-sol=deepseek")
	if cfg, err = config.Load(); err != nil || cfg.PromptVariant("gpt-5.6-sol") != "deepseek" || cfg.PromptVariant("gpt-5.6-terra") != "default" {
		t.Fatalf("override: %v", err)
	}
	t.Setenv("LLM_PROMPT_VARIANTS", "gpt-5.6-sol=fancy")
	if _, err = config.Load(); err == nil || !strings.Contains(err.Error(), "coding, deepseek, default, gpt-5.6") {
		t.Fatalf("unknown variant accepted: %v", err)
	}
}

func TestReasoningEffortsArePerModel(t *testing.T) {
	// GPT-5.6 accepts none…xhigh but not minimal (A-29); a run's effort is mapped to the nearest one the model accepts; non-reasoning models get none.
	// Arrange
	t.Setenv("LLM_MODELS", "gpt-5.6-sol,gpt-5.6-terra,other,plain")
	t.Setenv("LLM_REASONING_MODELS", "gpt-5.6-sol,gpt-5.6-terra,other")

	// Act
	cfg, err := config.Load()

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EffortAllowed("gpt-5.6-sol", "minimal") || !cfg.EffortAllowed("gpt-5.6-sol", "xhigh") || !cfg.EffortAllowed("other", "minimal") ||
		cfg.EffortAllowed("other", "xhigh") || cfg.EffortAllowed("plain", "low") || !cfg.EffortAllowed("plain", "") {
		t.Fatalf("efforts sol %v other %v", cfg.ReasoningEffortsOf("gpt-5.6-sol"), cfg.ReasoningEffortsOf("other"))
	}
	for _, c := range []struct{ model, in, want string }{
		{"gpt-5.6-sol", "minimal", "low"}, {"gpt-5.6-sol", "high", "high"}, {"other", "xhigh", "high"}, {"other", "none", "minimal"}, {"plain", "low", ""},
	} {
		if got := cfg.EffortFor(c.model, c.in); got != c.want {
			t.Errorf("EffortFor(%s, %s) = %q, want %q", c.model, c.in, got, c.want)
		}
	}
}

func TestUnsupportedEffort_OnlyReasoningModelsRefuseEfforts(t *testing.T) {
	// Arrange
	t.Setenv("LLM_MODELS", "gpt-5.6-sol,plain")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	// Act / Assert
	cases := []struct {
		model, effort string
		want          bool
	}{
		{"gpt-5.6-sol", "minimal", true}, // GPT-5.6 has no "minimal"
		{"gpt-5.6-sol", "xhigh", false},  // accepted
		{"gpt-5.6-sol", "", false},       // no effort asked
		{"gpt-5.6-sol", "bogus", true},   // not an effort at all
		{"plain", "minimal", false},      // non-reasoning models ignore efforts
		{"unlisted-model", "xhigh", false},
	}
	for _, tc := range cases {
		if got := cfg.UnsupportedEffort(tc.model, tc.effort); got != tc.want {
			t.Errorf("UnsupportedEffort(%q, %q) = %v, want %v", tc.model, tc.effort, got, tc.want)
		}
	}
}

func TestEffortFor_EdgeCases(t *testing.T) {
	// Arrange
	t.Setenv("LLM_MODELS", "r,plain")
	t.Setenv("LLM_REASONING_MODELS", "r")
	t.Setenv("LLM_REASONING_EFFORTS", "r=low|high")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	// Act / Assert
	for _, tc := range []struct{ model, in, want string }{
		{"r", "", ""},           // nothing asked, nothing sent
		{"r", "bogus", ""},      // an unknown effort cannot be mapped
		{"r", "medium", "high"}, // equidistant between low and high: the higher wins
		{"r", "none", "low"},    // below the scale
		{"r", "xhigh", "high"},  // above the scale
		{"r", "low", "low"},     // accepted as is
		{"plain", "", ""},       // not a reasoning model
		{"plain", "high", ""},
	} {
		if got := cfg.EffortFor(tc.model, tc.in); got != tc.want {
			t.Errorf("EffortFor(%q, %q) = %q, want %q", tc.model, tc.in, got, tc.want)
		}
	}
}

func TestLoadRejectsMalformedReasoningEffortsAndVariants(t *testing.T) {
	cases := map[string]struct{ name, value, want string }{
		"no separator":     {"LLM_REASONING_EFFORTS", "gpt", "is not model=effort"},
		"empty model":      {"LLM_REASONING_EFFORTS", "=low", "is not model=effort"},
		"empty effort set": {"LLM_REASONING_EFFORTS", "m=", "is not model=effort"},
		"unknown effort":   {"LLM_REASONING_EFFORTS", "m=low|extreme", `effort "extreme"`},
		"variant no model": {"LLM_PROMPT_VARIANTS", "=default", "is not model=variant"},
		"variant no equal": {"LLM_PROMPT_VARIANTS", "default", "is not model=variant"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)

			_, err := config.Load()

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
}
