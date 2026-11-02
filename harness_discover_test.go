package harness_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

func discoveringConfig(t *testing.T, baseURL, discover string) *config.Config {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_BASE_URL", baseURL)
	t.Setenv("LLM_API_KEY", "sk")
	t.Setenv("LLM_MODELS", "configured")
	t.Setenv("LLM_DEFAULT_MODEL", "configured")
	t.Setenv("LLM_DISCOVER_MODELS", discover)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newDiscovering(t *testing.T, cfg *config.Config) *harness.Harness {
	t.Helper()
	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: llmtest.New()}, Backend: sandboxtest.NewBackend()})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestDiscoveredModelsJoinTheAllowListAfterTheConfiguredOnes(t *testing.T) {
	// Arrange
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer sk" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"zeta"},{"id":"configured"},{"id":"alpha"}]}`))
	}))
	defer srv.Close()
	cfg := discoveringConfig(t, srv.URL+"/v1", "true")

	// Act
	h := newDiscovering(t, cfg)
	ctx := context.Background()
	th, _ := h.NewThread(ctx, "alice", "")
	_, err := h.Send(ctx, "alice", th.ID, "hi", harness.RunOptions{Model: "alpha"})

	// Assert
	if got := cfg.LLMModels; len(got) != 3 || got[0] != "configured" || got[1] != "alpha" || got[2] != "zeta" {
		t.Fatalf("models = %v", got)
	}
	if err != nil {
		t.Fatalf("a discovered model must be accepted by Send: %v", err)
	}
}

func TestAFailingProviderLeavesTheConfiguredModels(t *testing.T) {
	// Arrange
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusBadGateway) }))
	defer srv.Close()
	cfg := discoveringConfig(t, srv.URL+"/v1", "true")

	// Act
	newDiscovering(t, cfg)

	// Assert
	if got := cfg.LLMModels; len(got) != 1 || got[0] != "configured" {
		t.Fatalf("models = %v", got)
	}
}

func TestDiscoveryIsOffByDefault(t *testing.T) {
	// Arrange
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{"data":[{"id":"other"}]}`))
	}))
	defer srv.Close()
	cfg := discoveringConfig(t, srv.URL+"/v1", "")

	// Act
	newDiscovering(t, cfg)

	// Assert
	if called || len(cfg.LLMModels) != 1 {
		t.Fatalf("called %v models %v", called, cfg.LLMModels)
	}
}
