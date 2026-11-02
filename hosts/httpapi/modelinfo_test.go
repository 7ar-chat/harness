package httpapi_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/7ar-chat/harness/hosts/client"
	"github.com/7ar-chat/harness/hosts/httpapi"
)

func testInfo() client.ModelInfo {
	return client.ModelInfo{Provider: "openai", ContextWindow: 272000, MaxOutput: 128000, Priced: true,
		InputPerToken: 1.25e-6, OutputPerToken: 1e-5, CacheReadPerToken: 1.25e-7,
		Tier: &client.PriceTier{Above: 200000, Input: 2.5e-6}, Vision: true, Reasoning: true, Tools: true}
}

func TestModelsCarryTheInfoOfEachModelOverHTTP(t *testing.T) {
	want := testInfo()
	r := newRig(t, rigOpts{web: httpapi.Options{
		Token:     "s3cret",
		ModelInfo: func(id string) (client.ModelInfo, bool) { return want, id == "gpt-test" },
	}})
	c := client.HTTP(r.srv.URL, "s3cret")

	list, err := c.Models(context.Background())

	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(list.Info["gpt-test"], want) || len(list.Info) != 1 {
		t.Fatalf("info = %+v", list.Info)
	}
}

func TestModelsHaveNoInfoFieldWithoutALookup(t *testing.T) {
	r := newRig(t, rigOpts{})

	m := r.json("GET", "/api/models", "alice", "", 200)

	if _, has := m["info"]; has {
		t.Fatalf("models = %+v", m)
	}
}
