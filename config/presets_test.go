package config_test

import (
	"strings"
	"testing"

	"github.com/7ar-chat/harness/config"
)

func TestLoadDefaultsToFourThinkingPresets(t *testing.T) {
	// Without environment, the thinking slider has Terra for fast/balanced and Sol for thinking/deep (default balanced).
	// Arrange

	// Act
	cfg, err := config.Load()

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	want := []config.LLMPreset{
		{ID: "fast", Model: "gpt-5.6-terra", ReasoningEffort: "low"},
		{ID: "balanced", Model: "gpt-5.6-terra", ReasoningEffort: "medium"},
		{ID: "thinking", Model: "gpt-5.6-sol", ReasoningEffort: "medium"},
		{ID: "deep", Model: "gpt-5.6-sol", ReasoningEffort: "high"},
	}
	if len(cfg.LLMPresets) != len(want) || cfg.LLMDefaultPreset != "balanced" {
		t.Fatalf("presets %+v default %q", cfg.LLMPresets, cfg.LLMDefaultPreset)
	}
	for i, p := range want {
		if cfg.LLMPresets[i] != p {
			t.Fatalf("preset %d: got %+v want %+v", i, cfg.LLMPresets[i], p)
		}
	}
}

func TestLoadParsesThinkingPresets(t *testing.T) {
	// LLM_PRESETS is ordered id=model:effort (effort optional); "none" disables the slider; an explicit list is
	// validated strictly while the built-in default is dropped (PresetsSkipped) when LLM_MODELS lacks its models.
	cases := []struct {
		name    string
		env     map[string]string
		want    string // "id=model:effort,…" of the parsed presets
		def     string
		wantErr string
	}{
		{name: "custom order and default", env: map[string]string{"LLM_PRESETS": " quick = gpt-5.6-terra:none , max=gpt-5.6-sol:xhigh", "LLM_DEFAULT_PRESET": "max"},
			want: "quick=gpt-5.6-terra:none,max=gpt-5.6-sol:xhigh", def: "max"},
		{name: "effort the model does not accept", env: map[string]string{"LLM_PRESETS": "quick=gpt-5.6-terra:minimal"},
			wantErr: `effort "minimal" is not accepted by gpt-5.6-terra (none, low, medium, high, xhigh)`},
		{name: "no effort", env: map[string]string{"LLM_PRESETS": "a=gpt-5.6-terra,b=gpt-5.6-sol:", "LLM_DEFAULT_PRESET": "a"},
			want: "a=gpt-5.6-terra:,b=gpt-5.6-sol:", def: "a"},
		{name: "effortless preset on a non-reasoning model", env: map[string]string{"LLM_MODELS": "plain,gpt-5.6-terra", "LLM_REASONING_MODELS": "gpt-5.6-terra",
			"LLM_PRESETS": "plain=plain,balanced=gpt-5.6-terra:medium"}, want: "plain=plain:,balanced=gpt-5.6-terra:medium", def: "balanced"},
		{name: "explicitly none", env: map[string]string{"LLM_PRESETS": "none", "LLM_DEFAULT_PRESET": "missing"}, want: "", def: ""},
		{name: "empty value applies the default", env: map[string]string{"LLM_PRESETS": ""},
			want: "fast=gpt-5.6-terra:low,balanced=gpt-5.6-terra:medium,thinking=gpt-5.6-sol:medium,deep=gpt-5.6-sol:high", def: "balanced"},
		{name: "default presets dropped when the models differ", env: map[string]string{"LLM_MODELS": "a,b", "LLM_DEFAULT_MODEL": "a"}, want: "", def: ""},
		{name: "model not allowed", env: map[string]string{"LLM_PRESETS": "balanced=gpt-4o:low"}, wantErr: "not in LLM_MODELS"},
		{name: "bad effort", env: map[string]string{"LLM_PRESETS": "balanced=gpt-5.6-sol:extreme"}, wantErr: "must be one of none, minimal, low, medium, high, xhigh"},
		{name: "effort on a non-reasoning model", env: map[string]string{"LLM_REASONING_MODELS": "gpt-5.6-sol", "LLM_PRESETS": "balanced=gpt-5.6-terra:low"},
			wantErr: "not in LLM_REASONING_MODELS"},
		{name: "duplicate id", env: map[string]string{"LLM_PRESETS": "balanced=gpt-5.6-terra:low,balanced=gpt-5.6-sol:high"}, wantErr: "duplicate preset id"},
		{name: "id not a slug", env: map[string]string{"LLM_PRESETS": "Deep Think=gpt-5.6-sol:high", "LLM_DEFAULT_PRESET": "Deep Think"}, wantErr: "lowercase slug"},
		{name: "malformed entry", env: map[string]string{"LLM_PRESETS": "balanced"}, wantErr: "is not id=model:effort"},
		{name: "unknown default", env: map[string]string{"LLM_DEFAULT_PRESET": "turbo"}, wantErr: "LLM_DEFAULT_PRESET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			// Act
			cfg, err := config.Load()

			// Assert
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(cfg.LLMPresets))
			for _, p := range cfg.LLMPresets {
				got = append(got, p.ID+"="+p.Model+":"+p.ReasoningEffort)
			}
			if strings.Join(got, ",") != tc.want || cfg.LLMDefaultPreset != tc.def || (cfg.PresetsSkipped != "") != (tc.name == "default presets dropped when the models differ") {
				t.Fatalf("presets %q default %q", got, cfg.LLMDefaultPreset)
			}
		})
	}
}
