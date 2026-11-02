package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/config"
)

func TestLoadAppliesLoopAndContextDefaults(t *testing.T) {
	// Without environment: 90 s stream idle, 3 step retries, 20k spill, 6 unmasked tool results, 120 s MCP calls, and GPT-5.6 compacting at window − (max output + 20k) of a 272k window.
	// Arrange

	// Act
	cfg, err := config.Load()

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StreamIdle() != 90*time.Second || cfg.LLMStepRetries != 3 || cfg.ToolOutputSpillChars != 20000 || cfg.KeepRecentToolResults != 6 ||
		cfg.MCPCallTimeout() != 120*time.Second || cfg.CompactionThreshold != 0 {
		t.Fatalf("loop %+v threshold %v", cfg.Loop, cfg.CompactionThreshold)
	}
	for _, m := range []string{"gpt-5.6-sol", "gpt-5.6-terra"} {
		if cfg.ContextWindow(m) != 272000 || cfg.OutputReserve(m) != config.DefaultMaxOutputTokens+config.ReserveMarginTokens ||
			cfg.ReasoningPassback(m) || cfg.PromptVariant(m) != "gpt-5.6" || !cfg.PromptCacheKey(m) {
			t.Fatalf("%s: window %d reserve %d passback %v variant %q", m, cfg.ContextWindow(m), cfg.OutputReserve(m), cfg.ReasoningPassback(m), cfg.PromptVariant(m))
		}
	}
}

func TestLoadGivesDeepSeekModelsTheirDefaults(t *testing.T) {
	// A "deepseek/…" LLM_MODELS entry gets a 1M window, reasoning effort, reasoning passback, no vision, the deepseek prompt variant and no prompt_cache_key.
	// Arrange
	t.Setenv("LLM_MODELS", "gpt-5.6-terra,deepseek/DeepSeek-V4-Flash")

	// Act
	cfg, err := config.Load()

	// Assert
	m := "deepseek/DeepSeek-V4-Flash"
	if err != nil || cfg.ContextWindow(m) != 1_000_000 || !cfg.SupportsReasoning(m) || !cfg.ReasoningPassback(m) || cfg.SupportsVision(m) ||
		cfg.PromptVariant(m) != "deepseek" || cfg.PromptCacheKey(m) || cfg.ReasoningPassback("gpt-5.6-terra") {
		t.Fatalf("err %v window %d reasoning %v passback %v vision %v variant %q", err, cfg.ContextWindow(m), cfg.SupportsReasoning(m),
			cfg.ReasoningPassback(m), cfg.SupportsVision(m), cfg.PromptVariant(m))
	}
}

func TestLoadParsesLoopOverrides(t *testing.T) {
	// Explicit passback models and max output tokens replace the defaults; malformed ones are errors.
	// Arrange
	t.Setenv("LLM_MODELS", "a,deepseek/x")
	t.Setenv("LLM_REASONING_PASSBACK_MODELS", "a")
	t.Setenv("LLM_MAX_OUTPUT_TOKENS", "a=50000")

	// Act
	cfg, err := config.Load()
	t.Setenv("LLM_MAX_OUTPUT_TOKENS", "a=lots")
	_, badErr := config.Load()

	// Assert
	if err != nil || !cfg.ReasoningPassback("a") || cfg.ReasoningPassback("deepseek/x") || cfg.PromptVariant("a") != "default" ||
		cfg.PromptVariant("deepseek/x") != "deepseek" || cfg.OutputReserve("a") != 50000+config.ReserveMarginTokens || badErr == nil {
		t.Fatalf("err %v bad %v cfg %+v", err, badErr, cfg.Loop)
	}
}

func TestTheSpillThresholdCannotExceedTheToolOutputCap(t *testing.T) {
	// AGENT_TOOL_OUTPUT_SPILL_CHARS above AGENT_MAX_TOOL_OUTPUT_CHARS would cut outputs between the two in the middle without keeping them whole: such a configuration stops the service; equal values are fine.
	for name, c := range map[string]struct {
		spill, cap string
		fails      bool
	}{
		"above": {"40000", "30000", true},
		"equal": {"30000", "30000", false},
		"below": {"20000", "30000", false},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			t.Setenv("AGENT_TOOL_OUTPUT_SPILL_CHARS", c.spill)
			t.Setenv("AGENT_MAX_TOOL_OUTPUT_CHARS", c.cap)

			// Act
			_, err := config.Load()

			// Assert
			if (err != nil) != c.fails || (err != nil && !strings.Contains(err.Error(), "AGENT_TOOL_OUTPUT_SPILL_CHARS")) {
				t.Fatalf("err %v", err)
			}
		})
	}
}

func TestLoadReadsTheMaskFractionAndTheCallCeiling(t *testing.T) {
	// Round 3: masking starts at COMPACTION_MASK_FROM_FRACTION (default 0.5) of the window and a model call never lasts longer than LLM_CALL_MAX_SECONDS (default 1200); a fraction outside [0, 1) or a ceiling below 1 is refused.
	// Arrange

	// Act
	cfg, err := config.Load()
	t.Setenv("COMPACTION_MASK_FROM_FRACTION", "1.5")
	_, badFraction := config.Load()
	t.Setenv("COMPACTION_MASK_FROM_FRACTION", "0")
	t.Setenv("LLM_CALL_MAX_SECONDS", "0")
	_, badCeiling := config.Load()

	// Assert
	if err != nil || cfg.MaskFromFraction != 0.5 || cfg.CallCeiling() != 20*time.Minute {
		t.Fatalf("err %v fraction %v ceiling %v", err, cfg.MaskFromFraction, cfg.CallCeiling())
	}
	if badFraction == nil || !strings.Contains(badFraction.Error(), "COMPACTION_MASK_FROM_FRACTION") ||
		badCeiling == nil || !strings.Contains(badCeiling.Error(), "LLM_CALL_MAX_SECONDS") {
		t.Fatalf("fraction %v ceiling %v", badFraction, badCeiling)
	}
}
