package config

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/GhaziBenDahmane/harness/modelinfo"
)

// Names of the files in the user directory that belong to the model price table.
const (
	// ModelPricesEnv replaces the default source of the model price table
	// (LiteLLM's public JSON): a file path or an http(s) URL; "off" disables it.
	ModelPricesEnv = "HARNESS_MODEL_PRICES"
	// ModelPricesFile is the user's override, in the same schema: its models win
	// over the table's, so private or self-hosted models can be priced.
	ModelPricesFile = "model-prices.json"
	// CacheFolder holds downloaded data under the user directory.
	CacheFolder = "cache"
)

// ModelPriceOptions are the options to load the model price table, from
// HARNESS_MODEL_PRICES and the user directory. ok is false when the table is
// disabled ("off").
func ModelPriceOptions() (o modelinfo.Options, ok bool) {
	return modelPriceOptions(os.Getenv, UserDir())
}

func modelPriceOptions(get func(string) string, userDir string) (modelinfo.Options, bool) {
	var o modelinfo.Options
	if userDir != "" {
		o.CacheDir = filepath.Join(userDir, CacheFolder)
		o.Override = filepath.Join(userDir, ModelPricesFile)
	}
	switch src := strings.TrimSpace(get(ModelPricesEnv)); {
	case strings.EqualFold(src, "off"):
		return o, false
	case strings.HasPrefix(src, "http://"), strings.HasPrefix(src, "https://"):
		o.URL = src
	case src != "":
		o.File = src
	}
	return o, true
}
