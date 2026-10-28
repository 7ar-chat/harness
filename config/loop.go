package config

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Loop is the harness v3 loop & context configuration (docs/harness-v3.md §1,
// §2 spill, §5, §10). DeepSeek models ("deepseek/…" ids of LLM_MODELS) get
// their defaults (1M window, reasoning effort, reasoning passback, no
// prompt_cache_key) unless the lists name them; their prompt variant is
// stream C2's (LLM_PROMPT_VARIANTS).
type Loop struct {
	// LLMStreamIdleSeconds bounds the gap between two chunks of a model stream.
	LLMStreamIdleSeconds int `env:"LLM_STREAM_IDLE_SECONDS" envDefault:"90"`
	// LLMStepRetries is how often a failed model call is re-issued as a whole step.
	LLMStepRetries int `env:"LLM_STEP_RETRIES" envDefault:"3"`
	// ToolOutputSpillChars: longer tool outputs are written to /workspace/.work/tool-output/.
	ToolOutputSpillChars int `env:"AGENT_TOOL_OUTPUT_SPILL_CHARS" envDefault:"20000"`
	// KeepRecentToolResults: older tool results are masked in the model input (in-run masking).
	KeepRecentToolResults int `env:"COMPACTION_KEEP_RECENT_TOOL_RESULTS" envDefault:"6"`
	// MaskFromFraction: masking starts once the prompt passes this share of the
	// window (round 3, A1; 0: from the first step, as in v3).
	MaskFromFraction float64 `env:"COMPACTION_MASK_FROM_FRACTION" envDefault:"0.5"`
	// LLMCallMaxSeconds bounds one model call whatever the stream sends (round 3, N3).
	LLMCallMaxSeconds int `env:"LLM_CALL_MAX_SECONDS" envDefault:"1200"`
	// LLMMaxOutputTokensRaw is LLM_MAX_OUTPUT_TOKENS: "model=tokens,…", the output
	// reserved below the window (compaction at window − (max output + 20k)); not sent to the provider.
	LLMMaxOutputTokensRaw string `env:"LLM_MAX_OUTPUT_TOKENS"`
	LLMMaxOutputTokens    map[string]int
	// LLMReasoningPassbackModels get their reasoning_content back on assistant
	// messages (default: the DeepSeek models of LLM_MODELS).
	LLMReasoningPassbackModels []string `env:"LLM_REASONING_PASSBACK_MODELS" envSeparator:","`
	// LLMPromptCacheKey sends prompt_cache_key (the thread id) to the openai gateway provider.
	LLMPromptCacheKey bool `env:"LLM_PROMPT_CACHE_KEY" envDefault:"true"`
	// MCPCallTimeoutSeconds bounds one MCP tool call.
	MCPCallTimeoutSeconds int `env:"MCP_CALL_TIMEOUT_SECONDS" envDefault:"120"`
}

// Defaults of the output reserve and DeepSeek models.
const (
	DefaultMaxOutputTokens = 16000
	ReserveMarginTokens    = 20000
	DeepSeekContextWindow  = 1_000_000
)

// defaultReasoningModels is the envDefault of LLM_REASONING_MODELS (DeepSeek models join it).
const defaultReasoningModels = "gpt-5.6-sol,gpt-5.6-terra"

// IsDeepSeek reports a DeepSeek model id ("deepseek/DeepSeek-V4-Flash", or a bare "DeepSeek-…").
func IsDeepSeek(model string) bool {
	provider, name, ok := strings.Cut(model, "/")
	if !ok {
		name = provider
		provider = ""
	}
	return strings.EqualFold(provider, "deepseek") || strings.HasPrefix(strings.ToLower(name), "deepseek")
}

func (c *Config) normalizeLoop() error {
	if c.LLMStreamIdleSeconds < 1 {
		return fmt.Errorf("LLM_STREAM_IDLE_SECONDS must be ≥ 1, got %d", c.LLMStreamIdleSeconds)
	}
	if c.MaskFromFraction < 0 || c.MaskFromFraction >= 1 {
		return fmt.Errorf("COMPACTION_MASK_FROM_FRACTION must be in [0, 1) (0: mask from the first step), got %v", c.MaskFromFraction)
	}
	if c.LLMCallMaxSeconds < 1 {
		return fmt.Errorf("LLM_CALL_MAX_SECONDS must be ≥ 1, got %d", c.LLMCallMaxSeconds)
	}
	if c.LLMStepRetries < 0 {
		return fmt.Errorf("LLM_STEP_RETRIES must be ≥ 0, got %d", c.LLMStepRetries)
	}
	// Review round 1: an output between the spill threshold and the cap would be
	// cut in the middle without being kept whole (A-04).
	if c.AgentMaxToolOutputChars > 0 && c.ToolOutputSpillChars > c.AgentMaxToolOutputChars {
		return fmt.Errorf("AGENT_TOOL_OUTPUT_SPILL_CHARS (%d) must not exceed AGENT_MAX_TOOL_OUTPUT_CHARS (%d)", c.ToolOutputSpillChars, c.AgentMaxToolOutputChars)
	}
	var err error
	if c.LLMMaxOutputTokens, err = parseModelInts("LLM_MAX_OUTPUT_TOKENS", c.LLMMaxOutputTokensRaw, 256); err != nil {
		return err
	}
	c.LLMReasoningPassbackModels = trimAll(c.LLMReasoningPassbackModels)
	passbackDefault := len(c.LLMReasoningPassbackModels) == 0
	reasoningDefault := strings.Join(c.LLMReasoningModels, ",") == defaultReasoningModels
	for _, m := range c.LLMModels {
		if !IsDeepSeek(m) {
			continue
		}
		if _, ok := c.LLMContextWindows[m]; !ok {
			c.LLMContextWindows[m] = DeepSeekContextWindow
		}
		if reasoningDefault && !slices.Contains(c.LLMReasoningModels, m) {
			// DeepSeek returns reasoning_content only when reasoning_effort is sent.
			c.LLMReasoningModels = append(c.LLMReasoningModels, m)
		}
		if passbackDefault {
			c.LLMReasoningPassbackModels = append(c.LLMReasoningPassbackModels, m)
		}
	}
	return nil
}

func parseModelInts(name, raw string, minimum int) (map[string]int, error) {
	out := map[string]int{}
	for _, entry := range trimAll(strings.Split(raw, ",")) {
		model, tokens, ok := strings.Cut(entry, "=")
		n, err := strconv.Atoi(strings.TrimSpace(tokens))
		if !ok || strings.TrimSpace(model) == "" || err != nil || n < minimum {
			return nil, fmt.Errorf("%s: %q is not model=tokens (tokens ≥ %d)", name, entry, minimum)
		}
		out[strings.TrimSpace(model)] = n
	}
	return out, nil
}

// StreamIdle is LLM_STREAM_IDLE_SECONDS.
func (c *Config) StreamIdle() time.Duration {
	return time.Duration(c.LLMStreamIdleSeconds) * time.Second
}

// CallCeiling is LLM_CALL_MAX_SECONDS: no model call lasts longer, whatever its stream sends.
func (c *Config) CallCeiling() time.Duration {
	return time.Duration(c.LLMCallMaxSeconds) * time.Second
}

// MCPCallTimeout is MCP_CALL_TIMEOUT_SECONDS (≤ 0: 120 s).
func (c *Config) MCPCallTimeout() time.Duration {
	if c.MCPCallTimeoutSeconds <= 0 {
		return 120 * time.Second
	}
	return time.Duration(c.MCPCallTimeoutSeconds) * time.Second
}

// OutputReserve is the part of a model's window kept free below the compaction
// trigger: its max output tokens (LLM_MAX_OUTPUT_TOKENS, default 16000) + 20k.
func (c *Config) OutputReserve(model string) int {
	out := DefaultMaxOutputTokens
	if n, ok := c.LLMMaxOutputTokens[model]; ok {
		out = n
	}
	return out + ReserveMarginTokens
}

// ReasoningPassback reports whether a model's assistant messages carry their reasoning back.
func (c *Config) ReasoningPassback(model string) bool {
	return slices.Contains(c.LLMReasoningPassbackModels, model)
}

// PromptCacheKey reports whether calls of a model send prompt_cache_key: only
// models of the gateway's openai provider accept it (DeepSeek rejects unknown fields).
func (c *Config) PromptCacheKey(model string) bool {
	return c.LLMPromptCacheKey && !strings.Contains(model, "/")
}
