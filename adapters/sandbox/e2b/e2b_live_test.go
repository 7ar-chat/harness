package e2b_test

import (
	"os"
	"testing"

	"github.com/7ar-chat/harness/adapters/sandbox/e2b"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

// TestE2BConformance runs the adapter conformance suite against the real E2B service. It creates and kills sandboxes
// (each costs a few seconds of E2B compute). Opt in with E2B_API_KEY; E2B_TEMPLATE picks a template (default "base").
//
//	E2B_API_KEY=… go test ./e2b -run TestE2BConformance -v
func TestE2BConformance(t *testing.T) {
	key := os.Getenv("E2B_API_KEY")
	if key == "" {
		t.Skip("set E2B_API_KEY to run the live E2B conformance suite")
	}
	sandboxtest.RunConformance(t, func(t *testing.T) sandbox.Backend {
		b, err := e2b.New(e2b.Options{APIKey: key, Template: os.Getenv("E2B_TEMPLATE"), TimeoutSeconds: 600})
		if err != nil {
			t.Fatal(err)
		}
		return b
	})
}
