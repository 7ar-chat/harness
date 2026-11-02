package client_test

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"github.com/7ar-chat/harness/hosts/client"
	"github.com/7ar-chat/harness/modelinfo"
)

func TestModelListInfoSurvivesJSONAndIsOmittedWhenEmpty(t *testing.T) {
	in := client.ModelList{Models: []string{"a"}, Default: "a", Info: map[string]client.ModelInfo{
		"a": {ContextWindow: 200000, Priced: true, InputPerToken: 3e-6, Tier: &client.PriceTier{Above: 200000, Input: 6e-6}, Vision: true},
	}}

	b, _ := json.Marshal(in)
	var out client.ModelList
	err := json.Unmarshal(b, &out)
	plain, _ := json.Marshal(client.ModelList{Models: []string{"a"}, Default: "a"})

	if err != nil || !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip %s: %+v %v", b, out, err)
	}
	if string(plain) != `{"models":["a"],"default":"a"}` {
		t.Fatalf("an empty Info must not appear: %s", plain)
	}
}

func TestTableLookupConvertsAndPrefersTheNamedProvider(t *testing.T) {
	tb := &modelinfo.Table{}
	tb.Set(map[string]modelinfo.Info{
		"openrouter/x/m": {Provider: "openrouter", ContextWindow: 1, Priced: true, InputPerToken: 1e-6},
		"azure/x/m":      {Provider: "azure", ContextWindow: 2, Priced: true, InputPerToken: 2e-6},
	})

	lookup := client.TableLookup(tb, "azure")
	got, ok := lookup("m")
	_, miss := lookup("nothing")

	if !ok || got.ContextWindow != 2 || miss {
		t.Fatalf("got %+v %v miss %v", got, ok, miss)
	}
}

func TestModelInfoCostMatchesTheTable(t *testing.T) {
	m := client.ModelInfo{Priced: true, InputPerToken: 3e-6, OutputPerToken: 15e-6, CacheReadPerToken: 3e-7}

	usd, ok := m.Cost(100_000, 2_000, 60_000, 0)
	_, unpriced := (client.ModelInfo{}).Cost(10, 10, 0, 0)

	if !ok || math.Abs(usd-0.168) > 1e-9 || unpriced {
		t.Fatalf("cost %v %v unpriced %v", usd, ok, unpriced)
	}
}
