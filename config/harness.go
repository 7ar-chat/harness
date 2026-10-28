package config

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// DefaultContextWindow is the context window of models missing from LLM_CONTEXT_WINDOWS.
const DefaultContextWindow = 128000

// Harness is the harness v2 configuration (context management, model metadata).
type Harness struct {
	// The defaults describe Work's GPT-5.6 Sol / Terra (reasoning, vision; the
	// 272k input window Codex ships for these slugs — harness v3 §5, A-02).
	// LLMContextWindowsRaw is LLM_CONTEXT_WINDOWS: "model=tokens,…".
	LLMContextWindowsRaw string `env:"LLM_CONTEXT_WINDOWS" envDefault:"gpt-5.6-sol=272000,gpt-5.6-terra=272000"`
	LLMContextWindows    map[string]int
	// LLMReasoningModels accept reasoningEffort.
	LLMReasoningModels []string `env:"LLM_REASONING_MODELS" envDefault:"gpt-5.6-sol,gpt-5.6-terra" envSeparator:","`
	// LLMVisionModels accept image parts.
	LLMVisionModels []string `env:"LLM_VISION_MODELS" envDefault:"gpt-5.6-sol,gpt-5.6-terra" envSeparator:","`
	// LLMModelLabelsRaw is LLM_MODEL_LABELS: "model=Label,…" shown by /models (default: the id).
	LLMModelLabelsRaw string `env:"LLM_MODEL_LABELS" envDefault:"gpt-5.6-sol=GPT-5.6 Sol,gpt-5.6-terra=GPT-5.6 Terra"`
	LLMModelLabels    map[string]string
	// LLMPresetsRaw is LLM_PRESETS: the ordered stops of the composer's "thinking"
	// slider, "id=model:effort,…" (effort optional: "id=model" or "id=model:" keeps
	// the model's default effort). GPT-5.6 Sol costs about twice Terra (list
	// prices), so Terra covers the fast/balanced stops and Sol thinking/deep.
	// caarlos0/env applies envDefault to an empty value (like every list here), so
	// "none" is the explicit way to configure no presets (the UI then shows the
	// separate model and reasoning menus).
	// The built-in default is dropped (no slider, PresetsSkipped says why) when
	// LLM_MODELS / LLM_REASONING_MODELS lack its models, so deployments with other
	// models keep starting; an explicit LLM_PRESETS is always validated strictly.
	LLMPresetsRaw string `env:"LLM_PRESETS" envDefault:"fast=gpt-5.6-terra:low,balanced=gpt-5.6-terra:medium,thinking=gpt-5.6-sol:medium,deep=gpt-5.6-sol:high"` // = DefaultPresets
	LLMPresets    []LLMPreset
	// PresetsSkipped explains why the default LLM_PRESETS was dropped (logged at startup).
	PresetsSkipped string
	// LLMDefaultPreset is LLM_DEFAULT_PRESET: the stop of new threads (must be in LLM_PRESETS; ignored with "none").
	LLMDefaultPreset string `env:"LLM_DEFAULT_PRESET" envDefault:"balanced"`
	// CompactionThreshold is the share of the context window that triggers
	// compaction; unset (0): window − OutputReserve (harness v3 §5).
	CompactionThreshold float64 `env:"COMPACTION_THRESHOLD"`
	// CompactionModel summarizes old context (default: the run's model).
	CompactionModel string `env:"COMPACTION_MODEL"`
}

// LLMPreset is one stop of the thinking slider: a (model, reasoning effort) pair.
type LLMPreset struct {
	ID              string `json:"id"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort"`
}

// ReasoningEfforts are the reasoningEffort values accepted by POST /threads/{id}/runs
// (each model accepts a subset: Config.ReasoningEffortsOf, LLM_REASONING_EFFORTS).
var ReasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

// NoPresets is the LLM_PRESETS value that disables the thinking slider.
const NoPresets = "none"

// DefaultPresets is the envDefault of LLM_PRESETS.
const DefaultPresets = "fast=gpt-5.6-terra:low,balanced=gpt-5.6-terra:medium,thinking=gpt-5.6-sol:medium,deep=gpt-5.6-sol:high"

var presetIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// parsePresets validates LLM_PRESETS / LLM_DEFAULT_PRESET against LLM_MODELS and LLM_REASONING_MODELS.
func (c *Config) parsePresets() error {
	c.LLMPresets, c.PresetsSkipped = nil, ""
	c.LLMDefaultPreset = strings.TrimSpace(c.LLMDefaultPreset)
	raw := strings.TrimSpace(c.LLMPresetsRaw)
	if raw == "" || strings.EqualFold(raw, NoPresets) {
		c.LLMDefaultPreset = ""
		return nil
	}
	presets, err := c.parsePresetList(raw)
	if err != nil && raw == DefaultPresets {
		c.LLMDefaultPreset, c.PresetsSkipped = "", err.Error()
		return nil
	}
	if err != nil {
		return err
	}
	c.LLMPresets = presets
	if !slices.ContainsFunc(presets, func(p LLMPreset) bool { return p.ID == c.LLMDefaultPreset }) {
		return fmt.Errorf("LLM_DEFAULT_PRESET: %q is not a preset of LLM_PRESETS", c.LLMDefaultPreset)
	}
	return nil
}

func (c *Config) parsePresetList(raw string) ([]LLMPreset, error) {
	var presets []LLMPreset
	seen := map[string]bool{}
	for _, entry := range trimAll(strings.Split(raw, ",")) {
		id, spec, ok := strings.Cut(entry, "=")
		id, spec = strings.TrimSpace(id), strings.TrimSpace(spec)
		if !ok || spec == "" {
			return nil, fmt.Errorf("LLM_PRESETS: %q is not id=model:effort", entry)
		}
		if !presetIDPattern.MatchString(id) {
			return nil, fmt.Errorf("LLM_PRESETS: preset id %q must be a lowercase slug (a-z, 0-9, - or _, ≤ 32 chars)", id)
		}
		if seen[id] {
			return nil, fmt.Errorf("LLM_PRESETS: duplicate preset id %q", id)
		}
		seen[id] = true
		model, effort := spec, ""
		if i := strings.LastIndex(spec, ":"); i >= 0 {
			model, effort = strings.TrimSpace(spec[:i]), strings.TrimSpace(spec[i+1:])
		}
		if !c.ModelAllowed(model) {
			return nil, fmt.Errorf("LLM_PRESETS: preset %q uses model %q, which is not in LLM_MODELS", id, model)
		}
		if effort != "" {
			if !slices.Contains(ReasoningEfforts, effort) {
				return nil, fmt.Errorf("LLM_PRESETS: preset %q effort %q must be one of %s", id, effort, strings.Join(ReasoningEfforts, ", "))
			}
			if !c.SupportsReasoning(model) {
				return nil, fmt.Errorf("LLM_PRESETS: preset %q sets an effort but model %q is not in LLM_REASONING_MODELS", id, model)
			}
			if !c.EffortAllowed(model, effort) {
				return nil, fmt.Errorf("LLM_PRESETS: preset %q: effort %q is not accepted by %s (%s)", id, effort, model, strings.Join(c.ReasoningEffortsOf(model), ", "))
			}
		}
		presets = append(presets, LLMPreset{ID: id, Model: model, ReasoningEffort: effort})
	}
	return presets, nil
}

func (c *Config) normalizeHarness() error {
	c.LLMReasoningModels = trimAll(c.LLMReasoningModels)
	c.LLMVisionModels = trimAll(c.LLMVisionModels)
	c.CompactionModel = strings.TrimSpace(c.CompactionModel)
	if c.CompactionThreshold < 0 || c.CompactionThreshold >= 1 {
		return fmt.Errorf("COMPACTION_THRESHOLD must be between 0 and 1 (0 or unset: window − reserve), got %v", c.CompactionThreshold)
	}
	c.LLMContextWindows = map[string]int{}
	for _, entry := range trimAll(strings.Split(c.LLMContextWindowsRaw, ",")) {
		name, tokens, ok := strings.Cut(entry, "=")
		n, err := strconv.Atoi(strings.TrimSpace(tokens))
		if !ok || strings.TrimSpace(name) == "" || err != nil || n < 1000 {
			return fmt.Errorf("LLM_CONTEXT_WINDOWS: %q is not model=tokens (tokens ≥ 1000)", entry)
		}
		c.LLMContextWindows[strings.TrimSpace(name)] = n
	}
	c.LLMModelLabels = map[string]string{}
	for _, entry := range trimAll(strings.Split(c.LLMModelLabelsRaw, ",")) {
		name, label, ok := strings.Cut(entry, "=")
		if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(label) == "" {
			return fmt.Errorf("LLM_MODEL_LABELS: %q is not model=Label", entry)
		}
		c.LLMModelLabels[strings.TrimSpace(name)] = strings.TrimSpace(label)
	}
	if err := c.normalizeLoop(); err != nil {
		return err
	}
	return c.parsePresets()
}

// ModelLabel is the display name of a model (LLM_MODEL_LABELS, else its id).
func (c *Config) ModelLabel(model string) string {
	if label, ok := c.LLMModelLabels[model]; ok {
		return label
	}
	return model
}

// ContextWindow is the context window of a model in tokens.
func (c *Config) ContextWindow(model string) int {
	if n, ok := c.LLMContextWindows[model]; ok && n > 0 {
		return n
	}
	return DefaultContextWindow
}

// SupportsReasoning reports whether a model accepts reasoningEffort (LLM_REASONING_MODELS).
func (c *Config) SupportsReasoning(model string) bool {
	return slices.Contains(c.LLMReasoningModels, model)
}

// SupportsVision reports whether a model accepts image parts (LLM_VISION_MODELS).
func (c *Config) SupportsVision(model string) bool { return slices.Contains(c.LLMVisionModels, model) }

// Threshold is CompactionThreshold with its default when unset (tests build Config literals).
func (c *Config) Threshold() float64 {
	if c.CompactionThreshold <= 0 || c.CompactionThreshold >= 1 {
		return 0.75
	}
	return c.CompactionThreshold
}
