package opensandbox_test

import (
	"github.com/7ar-chat/harness/adapters/sandbox/opensandbox"
	"os"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

// TestOpenSandboxConformance runs the adapter conformance suite against a real
// OpenSandbox server. Opt in with OPENSANDBOX_URL (and OPENSANDBOX_API_KEY,
// OPENSANDBOX_IMAGE, SANDBOX_EGRESS_ALLOWLIST if the server needs them); the
// image must satisfy docs/sandbox-adapters.md (sandbox-image/Dockerfile does).
//
//	OPENSANDBOX_URL=http://localhost:8080 go test ./sandbox -run TestOpenSandboxConformance -v
func TestOpenSandboxConformance(t *testing.T) {
	url := os.Getenv("OPENSANDBOX_URL")
	if url == "" {
		t.Skip("set OPENSANDBOX_URL to run the live OpenSandbox conformance suite")
	}
	sandboxtest.RunConformance(t, func(t *testing.T) sandbox.Backend {
		o := opensandbox.OpenSandboxOptions{
			URL: url, APIKey: os.Getenv("OPENSANDBOX_API_KEY"), Image: os.Getenv("OPENSANDBOX_IMAGE"),
			CPU: "1", Memory: "2Gi", TimeoutSeconds: 600,
		}
		if v := strings.TrimSpace(os.Getenv("SANDBOX_EGRESS_ALLOWLIST")); v != "" {
			o.EgressAllowlist = strings.Split(v, ",")
		}
		if o.Image == "" {
			o.Image = "harness-sandbox:local"
		}
		b, err := opensandbox.NewOpenSandbox(o)
		if err != nil {
			t.Fatal(err)
		}
		return b
	})
}
