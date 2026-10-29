package modelinfo

import (
	"math"
	"os"
	"testing"
)

func fixtureTable(t *testing.T) *Table {
	t.Helper()
	data, err := os.ReadFile("testdata/prices.json")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	tb := &Table{}
	tb.Set(entries)
	return tb
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestParseReadsTheRealSchemaAndSkipsWhatIsNotAChatModel(t *testing.T) {
	tb := fixtureTable(t)

	i, ok := tb.Lookup("claude-sonnet-4-5", "")

	if !ok || i.ContextWindow != 200000 || i.MaxOutput != 64000 || i.Provider != "anthropic" || !i.Vision || !i.Reasoning || !i.Tools || !i.Priced {
		t.Fatalf("info %+v", i)
	}
	if !near(i.InputPerToken, 3e-6) || !near(i.OutputPerToken, 15e-6) || !near(i.CacheReadPerToken, 3e-7) || !near(i.CacheWritePerToken, 3.75e-6) {
		t.Fatalf("prices %+v", i)
	}
	for _, id := range []string{"sample_spec", "text-embedding-3-small", "not-an-object"} {
		if _, ok := tb.Lookup(id, ""); ok {
			t.Fatalf("%s should be skipped", id)
		}
	}
}

func TestParseToleratesJunkAndLegacyFields(t *testing.T) {
	tb := fixtureTable(t)

	junk, _ := tb.Lookup("junk-model", "")
	legacy, _ := tb.Lookup("legacy-model", "")
	free, _ := tb.Lookup("ollama/llama3", "")

	if junk.ContextWindow != 0 || junk.MaxOutput != 0 || junk.Priced || junk.Vision || junk.InputPerToken != 0 {
		t.Fatalf("junk %+v", junk)
	}
	if legacy.ContextWindow != 4096 || legacy.Priced {
		t.Fatalf("legacy %+v", legacy)
	}
	if !free.Priced || free.InputPerToken != 0 || free.ContextWindow != 8192 {
		t.Fatalf("free %+v", free)
	}
}

func TestParseRejectsNonJSONAndEmptyTables(t *testing.T) {
	for _, in := range []string{"not json", `[]`, `{}`, `{"sample_spec":{}}`} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Fatalf("%q parsed", in)
		}
	}
}

func TestLookupPrecedence(t *testing.T) {
	tb := fixtureTable(t)
	cases := []struct {
		name, id, hint, want string
	}{
		{"exact beats a hint form", "gpt-5", "azure_ai", "gpt-5"},
		{"provider-prefixed form", "gpt-5-mini", "openrouter", "gpt-5-mini"},
		{"hint prefix when no exact entry", "deepseek/deepseek-r1", "openrouter", "openrouter/deepseek/deepseek-r1"},
		{"suffix after the slash", "deepseek-r1", "", "openrouter/deepseek/deepseek-r1"},
		{"foreign prefix falls back to the suffix", "somehost/gpt-5-mini", "", "gpt-5-mini"},
		{"bedrock spelling of a dated id", "claude-sonnet-4-20250514", "anthropic", "anthropic.claude-sonnet-4-20250514-v1:0"},
		{"dated id of an undated entry", "claude-sonnet-4-5-20250929", "", "claude-sonnet-4-5"},
		{"undated id of a dated entry", "claude-opus-4-5", "", "claude-opus-4-5-20251101"},
		{"case does not matter for normalized ids", "Claude-Opus-4-5", "", "claude-opus-4-5-20251101"},
	}
	for _, c := range cases {
		got, ok := tb.Lookup(c.id, c.hint)
		if !ok || got.ID != c.want {
			t.Errorf("%s: Lookup(%q, %q) = %q, %v; want %q", c.name, c.id, c.hint, got.ID, ok, c.want)
		}
	}
}

func TestLookupNeverGuessesAcrossModels(t *testing.T) {
	tb := fixtureTable(t)

	for _, id := range []string{"gpt-4", "gpt", "claude", "claude-sonnet", "gpt-5-mini-high", "gpt-5-turbo", "llama3-70b", ""} {
		if i, ok := tb.Lookup(id, ""); ok {
			t.Errorf("Lookup(%q) = %q", id, i.ID)
		}
	}
}

func TestLookupOnAnEmptyTable(t *testing.T) {
	var nilTable *Table

	if _, ok := nilTable.Lookup("gpt-5", ""); ok || nilTable.Ready() {
		t.Fatal("nil table answered")
	}
	if _, ok := (&Table{}).Lookup("gpt-5", ""); ok {
		t.Fatal("empty table answered")
	}
}

func TestSetReplacesAModelAndDropsMemoizedMisses(t *testing.T) {
	tb := fixtureTable(t)
	if _, ok := tb.Lookup("my-model", ""); ok {
		t.Fatal("unexpected hit")
	}

	tb.Set(map[string]Info{"my-model": {Provider: "local", ContextWindow: 32000, Priced: true}, "gpt-5": {ContextWindow: 1}})

	if i, ok := tb.Lookup("my-model", ""); !ok || i.ContextWindow != 32000 {
		t.Fatalf("my-model %+v %v", i, ok)
	}
	if i, _ := tb.Lookup("gpt-5", ""); i.ContextWindow != 1 {
		t.Fatalf("gpt-5 %+v", i)
	}
}

func TestCostBillsCachedTokensAtTheCacheReadPrice(t *testing.T) {
	i := Info{Priced: true, InputPerToken: 3e-6, OutputPerToken: 15e-6, CacheReadPerToken: 3e-7}

	usd, ok := Cost(i, Usage{In: 100_000, Out: 2_000, Cached: 60_000})

	// 40k fresh * 3e-6 + 60k cached * 3e-7 + 2k out * 15e-6
	if !ok || !near(usd, 0.12+0.018+0.03) {
		t.Fatalf("cost %v %v", usd, ok)
	}
}

func TestCostFallsBackToTheInputPriceForCachedTokens(t *testing.T) {
	i := Info{Priced: true, InputPerToken: 2e-6, OutputPerToken: 4e-6}

	usd, _ := Cost(i, Usage{In: 1000, Out: 500, Cached: 400})

	if !near(usd, 1000*2e-6+500*4e-6) {
		t.Fatalf("cost %v", usd)
	}
}

func TestCostClampsCachedTokensToTheInput(t *testing.T) {
	i := Info{Priced: true, InputPerToken: 1e-6, CacheReadPerToken: 1e-7}

	usd, _ := Cost(i, Usage{In: 100, Cached: 900})

	if !near(usd, 100*1e-7) {
		t.Fatalf("cost %v", usd)
	}
}

func TestCostUsesTheTierForPromptsAboveItsLimit(t *testing.T) {
	tb := fixtureTable(t)
	i, _ := tb.Lookup("claude-sonnet-4-20250514", "")

	base, _ := Cost(i, Usage{In: 1_000_000, Out: 1_000_000, Prompt: 150_000})
	tier, _ := Cost(i, Usage{In: 1_000_000, Out: 1_000_000, Prompt: 250_000})
	unknown, _ := Cost(i, Usage{In: 1_000_000, Out: 1_000_000})

	if !near(base, 3+15) || !near(tier, 6+22.5) || !near(unknown, base) {
		t.Fatalf("base %v tier %v unknown %v", base, tier, unknown)
	}
}

func TestCostOfAnUnpricedModelIsUnknownAndOfAFreeModelIsZero(t *testing.T) {
	tb := fixtureTable(t)
	junk, _ := tb.Lookup("junk-model", "")
	free, _ := tb.Lookup("ollama/llama3", "")

	_, junkOK := Cost(junk, Usage{In: 1000})
	usd, freeOK := Cost(free, Usage{In: 1000, Out: 1000})

	if junkOK || !freeOK || usd != 0 {
		t.Fatalf("junk %v free %v %v", junkOK, usd, freeOK)
	}
}
