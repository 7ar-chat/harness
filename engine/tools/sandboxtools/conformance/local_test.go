//go:build unix

package conformance_test

import (
	"testing"

	"github.com/GhaziBenDahmane/harness/engine/tools/sandboxtools/conformance"
	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/sandbox/local"
)

// The local backend answers the job scripts natively and runs search_files'
// python fallback or ripgrep, so this runs on a developer machine too.
func TestToolsConformanceOnTheLocalBackend(t *testing.T) {
	conformance.Run(t, func(t *testing.T) sandbox.Backend {
		b, err := local.New(local.Options{StateDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		return b
	})
}
