package daytona_test

import (
	"os"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/adapters/sandbox/daytona"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

// TestDaytonaConformance runs the adapter conformance suite against a real Daytona account. Opt in with
// DAYTONA_API_KEY; DAYTONA_API_URL, DAYTONA_TARGET and DAYTONA_SNAPSHOT are optional. The snapshot must satisfy
// docs/sandbox-adapters.md section 3 (bash, GNU coreutils, setsid, python3); build one from sandbox-image/Dockerfile.
// It creates a few real sandboxes and deletes them.
//
//	DAYTONA_API_KEY=... DAYTONA_SNAPSHOT=harness-sandbox go test ./daytona -run TestDaytonaConformance -v
func TestDaytonaConformance(t *testing.T) {
	key := os.Getenv("DAYTONA_API_KEY")
	if key == "" {
		t.Skip("set DAYTONA_API_KEY to run the live Daytona conformance suite")
	}
	sandboxtest.RunConformance(t, func(t *testing.T) sandbox.Backend {
		o := daytona.Options{APIKey: key, URL: os.Getenv("DAYTONA_API_URL"), Target: os.Getenv("DAYTONA_TARGET"),
			Snapshot: os.Getenv("DAYTONA_SNAPSHOT"), TimeoutSeconds: 600}
		if v := strings.TrimSpace(os.Getenv("SANDBOX_EGRESS_ALLOWLIST")); v != "" {
			o.EgressAllowlist = strings.Split(v, ",")
		}
		b, err := daytona.New(o)
		if err != nil {
			t.Fatal(err)
		}
		return b
	})
}
