package config

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/GhaziBenDahmane/harness/engine/agent/prompts"
)

// ---- harness v3, stream C2 (prompt variants, reasoning efforts per model) ----

// PromptAndEfforts is the prompt variant and reasoning effort configuration (harness v3 §6, A-29).
type PromptAndEfforts struct {
	// LLMPromptVariantsRaw is LLM_PROMPT_VARIANTS: "model=variant,…" with the
	// variants of internal/agent/prompts (default, gpt-5.6, deepseek, coding).
	// Models not listed use deepseek when their name contains "deepseek",
	// else default.
	// AgentProfile is AGENT_PROFILE: "work" (default, documents and data), "minimal" (sandbox and plan tools only) or "coding": models not
	// listed in an explicit LLM_PROMPT_VARIANTS then use the coding prompt, which also loads AGENTS.md.
	AgentProfile         string `env:"AGENT_PROFILE"`
	LLMPromptVariantsRaw string `env:"LLM_PROMPT_VARIANTS" envDefault:"gpt-5.6-sol=gpt-5.6,gpt-5.6-terra=gpt-5.6"`
	LLMPromptVariants    map[string]string
	// LLMReasoningEffortsRaw is LLM_REASONING_EFFORTS: "model=effort|effort…,…",
	// the reasoningEffort values a model accepts (GPT-5.x from 5.1: no
	// "minimal"; "none" and "xhigh" exist). Reasoning models not listed accept
	// DefaultReasoningEfforts.
	LLMReasoningEffortsRaw string `env:"LLM_REASONING_EFFORTS" envDefault:"gpt-5.6-sol=none|low|medium|high|xhigh,gpt-5.6-terra=none|low|medium|high|xhigh"`
	LLMReasoningEfforts    map[string][]string
}

// DefaultReasoningEfforts are accepted by reasoning models missing from LLM_REASONING_EFFORTS.
var DefaultReasoningEfforts = []string{"minimal", "low", "medium", "high"}

// effortScale orders every known effort, lowest first (nearest-effort mapping).
var effortScale = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

func (c *Config) normalizePromptAndEfforts() error {
	switch strings.ToLower(strings.TrimSpace(c.AgentProfile)) {
	case "", "work", "coding", "minimal":
	default:
		return fmt.Errorf("AGENT_PROFILE must be work, coding or minimal, got %q", c.AgentProfile)
	}
	c.LLMPromptVariants = map[string]string{}
	for _, entry := range trimAll(strings.Split(c.LLMPromptVariantsRaw, ",")) {
		model, variant, ok := strings.Cut(entry, "=")
		model, variant = strings.TrimSpace(model), strings.TrimSpace(variant)
		if !ok || model == "" || !prompts.Known(variant) {
			return fmt.Errorf("LLM_PROMPT_VARIANTS: %q is not model=variant (variants: %s)", entry, strings.Join(prompts.Names(), ", "))
		}
		c.LLMPromptVariants[model] = variant
	}
	if strings.EqualFold(strings.TrimSpace(c.AgentProfile), "coding") {
		if _, set := os.LookupEnv("LLM_PROMPT_VARIANTS"); !set {
			c.LLMPromptVariants = map[string]string{} // the work defaults do not apply to the coding profile
		}
	}
	c.LLMReasoningEfforts = map[string][]string{}
	for _, entry := range trimAll(strings.Split(c.LLMReasoningEffortsRaw, ",")) {
		model, list, ok := strings.Cut(entry, "=")
		model = strings.TrimSpace(model)
		efforts := trimAll(strings.Split(list, "|"))
		if !ok || model == "" || len(efforts) == 0 {
			return fmt.Errorf("LLM_REASONING_EFFORTS: %q is not model=effort|effort…", entry)
		}
		for _, e := range efforts {
			if !slices.Contains(effortScale, e) {
				return fmt.Errorf("LLM_REASONING_EFFORTS: %q: effort %q must be one of %s", entry, e, strings.Join(effortScale, ", "))
			}
		}
		c.LLMReasoningEfforts[model] = efforts
	}
	return nil
}

// PromptVariant is the system prompt variant of a model.
func (c *Config) PromptVariant(model string) string {
	if v, ok := c.LLMPromptVariants[model]; ok {
		return v
	}
	if strings.EqualFold(strings.TrimSpace(c.AgentProfile), "coding") {
		return prompts.Coding
	}
	if strings.Contains(strings.ToLower(model), "deepseek") {
		return prompts.DeepSeek
	}
	return prompts.Default
}

// ReasoningEffortsOf lists the reasoningEffort values a model accepts (nil: none, not a reasoning model).
func (c *Config) ReasoningEffortsOf(model string) []string {
	if !c.SupportsReasoning(model) {
		return nil
	}
	if list, ok := c.LLMReasoningEfforts[model]; ok {
		return list
	}
	return DefaultReasoningEfforts
}

// EffortAllowed reports whether a run may ask model for effort ("" always may).
func (c *Config) EffortAllowed(model, effort string) bool {
	if effort == "" {
		return true
	}
	return slices.Contains(c.ReasoningEffortsOf(model), effort)
}

// UnsupportedEffort reports a run effort a reasoning model does not accept
// (non-reasoning models ignore efforts: never unsupported).
func (c *Config) UnsupportedEffort(model, effort string) bool {
	return effort != "" && c.SupportsReasoning(model) && !c.EffortAllowed(model, effort)
}

// EffortFor is the effort actually sent for a run's effort: itself when the
// model accepts it, else the nearest accepted one (a tie goes to the higher
// effort; e.g. "minimal" becomes "low" on GPT-5.6), "" for other models.
func (c *Config) EffortFor(model, effort string) string {
	list := c.ReasoningEffortsOf(model)
	if effort == "" || len(list) == 0 || slices.Contains(list, effort) {
		if len(list) == 0 {
			return ""
		}
		return effort
	}
	at := slices.Index(effortScale, effort)
	if at < 0 {
		return ""
	}
	best, dist := "", len(effortScale)
	for _, e := range list {
		d := slices.Index(effortScale, e) - at
		if d < 0 {
			d = -d*2 + 1 // lower efforts lose ties
		} else {
			d *= 2
		}
		if d < dist {
			best, dist = e, d
		}
	}
	return best
}
