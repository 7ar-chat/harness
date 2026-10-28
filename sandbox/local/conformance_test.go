//go:build unix

package local_test

import (
	"testing"

	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/sandbox/local"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
)

// The local backend must pass the adapter conformance suite. (The Protocol
// scripts are answered natively here, so the macOS CI needs no GNU tools.)
func TestConformance(t *testing.T) {
	sandboxtest.RunConformanceWith(t, func(t *testing.T) sandbox.Backend {
		// No shared Workspace: every sandbox gets a directory of its own.
		b, err := local.New(local.Options{StateDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}, sandboxtest.ConformanceOptions{
		Confined: true,
		Skip: map[string]string{
			"Files/PrepareWorkspaceBuildsTheLayout": "local keeps /workspace/outputs out of the user's project on purpose (MkdirAll is a no-op for it)",
		},
	})
}
