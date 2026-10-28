package compaction_test

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/GhaziBenDahmane/harness/engine/compaction"
)

// gatewayTokens are the prompt tokens gpt-5.6 reported for each sample through
// the llm-gateway, minus the 6-token request overhead (docs/spike-responses-api.md).
var gatewayTokens = map[string]int{
	"english": 351, "french": 392, "russian": 460, "greek": 639,
	"json": 724, "python": 395, "chinese": 372, "emoji": 321,
}

func TestEstimateTextIsWithin15PercentOfTheProviderCount(t *testing.T) {
	// The pre-call token estimate stays within 15 % of gpt-5.6's real count for prose in five scripts, JSON, code and emoji (A-30).
	// Arrange
	samples := map[string]string{}
	for name := range gatewayTokens {
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
	for name, want := range gatewayTokens {
		if dev := math.Abs(float64(got[name]-want)) / float64(want); dev > 0.15 {
			t.Errorf("%s: estimate %d, provider %d (%.0f %% off)", name, got[name], want, 100*dev)
		}
	}
}
