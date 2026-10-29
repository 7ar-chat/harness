package config

import (
	"path/filepath"
	"testing"
)

func TestModelPriceOptionsFollowTheEnvironment(t *testing.T) {
	user := filepath.Join("home", ".har")
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == ModelPricesEnv {
				return v
			}
			return ""
		}
	}

	def, defOK := modelPriceOptions(env(""), user)
	url, urlOK := modelPriceOptions(env("https://example.test/prices.json"), user)
	file, fileOK := modelPriceOptions(env("/etc/prices.json"), user)
	_, offOK := modelPriceOptions(env("OFF"), user)
	noHome, _ := modelPriceOptions(env(""), "")

	if !defOK || def.URL != "" || def.File != "" || def.CacheDir != filepath.Join(user, "cache") || def.Override != filepath.Join(user, "model-prices.json") {
		t.Fatalf("default %+v", def)
	}
	if !urlOK || url.URL != "https://example.test/prices.json" || url.File != "" {
		t.Fatalf("url %+v", url)
	}
	if !fileOK || file.File != "/etc/prices.json" || file.URL != "" {
		t.Fatalf("file %+v", file)
	}
	if offOK {
		t.Fatal("off did not disable the table")
	}
	if noHome.CacheDir != "" || noHome.Override != "" {
		t.Fatalf("without a user directory nothing is written or read there: %+v", noHome)
	}
}
