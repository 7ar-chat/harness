package recipes_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/recipes"
)

func TestEveryBuiltinRecipeIsValidAndRendersWithItsDefaults(t *testing.T) {
	list := recipes.Builtins()
	if len(list) < 12 {
		t.Fatalf("want at least 12 builtin recipes, got %d", len(list))
	}
	seen := map[string]bool{}
	for _, r := range list {
		if seen[r.ID] {
			t.Errorf("%s: duplicate id", r.ID)
		}
		seen[r.ID] = true
		if err := r.Validate(); err != nil {
			t.Errorf("%s: %v", r.ID, err)
			continue
		}
		given := map[string]string{}
		for _, p := range r.Params {
			if p.Default == "" && p.Required {
				given[p.Key] = "internal/billing"
			}
		}
		out, err := r.Apply(given)
		if err != nil {
			t.Errorf("%s: %v", r.ID, err)
			continue
		}
		if strings.Contains(out.Body.Prompt, "{{") && r.Kind != recipes.KindTrigger {
			t.Errorf("%s: a placeholder was left in the prompt", r.ID)
		}
		if r.Suggest.MaxUSD <= 0 {
			t.Errorf("%s: no budget cap", r.ID)
		}
		if r.Kind == recipes.KindSchedule && r.Suggest.Policy != "readonly" {
			t.Errorf("%s: a scheduled recipe must be read-only", r.ID)
		}
	}
}

func sample() recipes.Recipe {
	return recipes.Recipe{Name: "Nightly", Category: recipes.CatReporting, Kind: recipes.KindSchedule,
		Body: recipes.Body{Name: "Report {{topic}}", Prompt: "Summarise {{topic}} for {{days}} days.", Cron: "{{when}}"},
		Params: []recipes.Param{
			{Key: "topic", Label: "Topic", Type: recipes.TypeText, Required: true},
			{Key: "days", Label: "Days", Type: recipes.TypeNumber, Default: "7"},
			{Key: "when", Label: "When", Type: recipes.TypeCron, Default: "0 2 * * *"},
		}}
}

func TestTemplatingFillsOnlyDeclaredParametersInOnePass(t *testing.T) {
	r := sample()
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	// A value that looks like a placeholder is plain text, never expanded again.
	out, err := r.Apply(map[string]string{"topic": "{{days}} and {{when}}"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.Prompt != "Summarise {{days}} and {{when}} for 7 days." || out.Body.Cron != "0 2 * * *" {
		t.Fatalf("got %q / %q", out.Body.Prompt, out.Body.Cron)
	}
}

func TestTemplatingRefusesWhatIsNotSafe(t *testing.T) {
	cases := map[string]map[string]string{
		"unknown parameter":   {"topic": "x", "extra": "y"},
		"required is missing": {},
		"not a number":        {"topic": "x", "days": "seven"},
		"too long":            {"topic": strings.Repeat("a", recipes.MaxValue+1)},
		"control character":   {"topic": "a\x00b"},
		"bad cron":            {"topic": "x", "when": "every night"},
	}
	for name, given := range cases {
		if _, err := sample().Apply(given); !errors.Is(err, recipes.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	// a line break must not reach a one-line field
	if _, err := sample().Apply(map[string]string{"topic": "a\nb"}); !errors.Is(err, recipes.ErrInvalid) {
		t.Errorf("line break in the name: %v", err)
	}
}

func TestValidateRefusesUndeclaredPlaceholdersAndBadShapes(t *testing.T) {
	r := sample()
	r.Body.Prompt += " {{nope}}"
	if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("undeclared placeholder: %v", err)
	}
	r = sample()
	r.Body.Prompt += " {{Topic}}"
	if err := r.Validate(); err == nil {
		t.Error("an upper case look-alike must be refused")
	}
	r = sample()
	r.Kind = recipes.KindTrigger
	if err := r.Validate(); err == nil {
		t.Error("a trigger needs a title")
	}
	r = sample()
	r.Params = append(r.Params, r.Params[0])
	if err := r.Validate(); err == nil {
		t.Error("duplicate key")
	}
	r = sample()
	r.Params[1].Default = "x"
	if err := r.Validate(); err == nil {
		t.Error("a default must fit the type")
	}
}

func TestTriggerValuesCannotInjectTemplateActions(t *testing.T) {
	r := recipes.Recipe{Name: "T", Category: recipes.CatMaintenance, Kind: recipes.KindTrigger,
		Body:   recipes.Body{Name: "t", Event: "issues.opened", Title: "Triage {{.title}}", Prompt: "Labels: {{labels}} for {{.URL}}"},
		Params: []recipes.Param{{Key: "labels", Label: "Labels", Type: recipes.TypeText, Required: true}}}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	out, err := r.Apply(map[string]string{"labels": "{{.Secret}}"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Body.Prompt != `Labels: {{"{{"}}.Secret}} for {{.URL}}` {
		t.Fatalf("got %q", out.Body.Prompt)
	}
}
