package harness_test

import (
	"strings"
	"testing"

	"github.com/7ar-chat/harness/adapters/sandbox/backends"
	"github.com/7ar-chat/harness/adapters/sandbox/daytona"
	"github.com/7ar-chat/harness/adapters/sandbox/e2b"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
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

func TestHostedBackendsAreSelectedByNameAndReceiveTheirSettingsAndImages(t *testing.T) {
	for _, c := range []struct {
		backend string
		env     map[string]string
		images  func(backends.Settings) map[string]string
	}{
		{daytona.Name, map[string]string{"DAYTONA_API_KEY": "dtn_test", "DAYTONA_SNAPSHOT": "default-snap"}, func(s backends.Settings) map[string]string { return s.Daytona.Images }},
		{e2b.Name, map[string]string{"E2B_API_KEY": "e2b_test", "E2B_TEMPLATE": "default-tpl"}, func(s backends.Settings) map[string]string { return s.E2B.Images }},
	} {
		t.Run(c.backend, func(t *testing.T) {
			// Arrange
			env := map[string]string{"SANDBOX_BACKEND": c.backend, "SANDBOX_IMAGES": "work=office-image,coding=lean-image"}
			for k, v := range c.env {
				env[k] = v
			}
			cfg := featureConfig(t, env)

			// Act
			b, err := harness.NewSandboxBackend(cfg)

			// Assert
			if err != nil || b == nil {
				t.Fatalf("NewSandboxBackend = %v, %v", b, err)
			}
			if got := c.images(harness.SandboxSettings(cfg)); got["work"] != "office-image" || got["coding"] != "lean-image" {
				t.Fatalf("images reaching the backend: %v", got)
			}
		})
	}
}

func TestAHostedBackendWithoutItsKeyFailsWithAnActionableMessage(t *testing.T) {
	cfg := featureConfig(t, map[string]string{"SANDBOX_BACKEND": "daytona"})

	_, err := harness.NewSandboxBackend(cfg)

	if err == nil || !strings.Contains(err.Error(), "DAYTONA_API_KEY") {
		t.Fatalf("err = %v", err)
	}
}
