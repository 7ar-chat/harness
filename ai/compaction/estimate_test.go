package compaction_test

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/GhaziBenDahmane/harness/ai/compaction"
)

// providerTokens are the prompt tokens an o200k model reported for each sample, minus the 6-token request overhead.
var providerTokens = map[string]int{
	"english": 351, "french": 392, "russian": 460, "greek": 639,
	"json": 724, "python": 395, "chinese": 372, "emoji": 321,
}

func TestEstimateTextIsWithin15PercentOfTheProviderCount(t *testing.T) {
	// The pre-call token estimate stays within 15 % of the provider's real count for prose in five scripts, JSON, code and emoji.
	// Arrange
	samples := map[string]string{}
	for name := range providerTokens {
		b, err := os.ReadFile(filepath.Join("testdata", "tokens", name+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		samples[name] = string(b)
	}

	// Act
	got := map[string]int{}
	for name, text := range samples {
		got[name] = compaction.EstimateText(text)
	}

	// Assert
	for name, want := range providerTokens {
		if dev := math.Abs(float64(got[name]-want)) / float64(want); dev > 0.15 {
			t.Errorf("%s: estimate %d, provider %d (%.0f %% off)", name, got[name], want, 100*dev)
		}
	}
}
