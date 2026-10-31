package harness_test

import (
	"github.com/GhaziBenDahmane/harness/adapters/sandbox/backends"
	"testing"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
)

func TestSandboxBackendSelectsARegisteredAdapter(t *testing.T) {
	// Arrange: a host registers its adapter, the environment names it
	backends.Register("harness-registry-test", func(backends.Settings) (sandbox.Backend, error) { return sandboxtest.NewBackend(), nil })
	cfg := featureConfig(t, map[string]string{"SANDBOX_BACKEND": "harness-registry-test"})

	// Act
	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: llmtest.New()}})

	// Assert
	if err != nil || !h.HasSandbox() {
		t.Fatalf("New: %v, HasSandbox %v", err, h != nil && h.HasSandbox())
	}
	if !cfg.SandboxEnabled() {
		t.Fatal("config.SandboxEnabled is false for a registered backend")
	}
}

func TestAnUnknownSandboxBackendFailsNew(t *testing.T) {
	cfg := featureConfig(t, map[string]string{"SANDBOX_BACKEND": "nope"})
	if _, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: llmtest.New()}}); err == nil {
		t.Fatal("expected an error")
	}
}
