package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/hosts/httpapi"
)

// remoteServer serves a scripted harness over HTTP behind a bearer token, as harness-web does.
func remoteServer(t *testing.T, ctx context.Context, turns ...llmtest.Turn) *httptest.Server {
	t.Helper()
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: llmtest.New(turns...)}, Backend: sandboxtest.NewBackend()})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = h.Run(ctx) }()
	srv := httptest.NewServer(httpapi.New(h, httpapi.Options{Token: "s3cret", Models: cfg.LLMModels, DefaultModel: "gpt-test"}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRemotePrintModeRunsOnTheServerAndUsesItsDefaultModel(t *testing.T) {
	// Arrange
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv := remoteServer(t, ctx, llmtest.Turn{Chunks: llmtest.Text("hi ", "from afar")})
	t.Setenv("HARNESS_REMOTE_TOKEN", "s3cret")
	var stdout, stderr bytes.Buffer

	// Act: no -model, no engine configuration of its own
	code := run(ctx, []string{"-remote", srv.URL, "-p", "hello"}, strings.NewReader(""), &stdout, &stderr, false, nil)

	// Assert
	if code != 0 || stdout.String() != "hi from afar\n" {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestRemoteWithAWrongTokenFails(t *testing.T) {
	// Arrange
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv := remoteServer(t, ctx)
	t.Setenv("HARNESS_REMOTE_TOKEN", "nope")
	var stdout, stderr bytes.Buffer

	// Act
	code := run(ctx, []string{"-remote", srv.URL, "-p", "hello"}, strings.NewReader(""), &stdout, &stderr, false, nil)

	// Assert
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "unauthenticated") {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestRemoteRefusesUndo(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run(context.Background(), []string{"-remote", "http://127.0.0.1:1", "-undo"}, strings.NewReader(""), &stdout, &stderr, true, nil)

	if code != 1 || !strings.Contains(stderr.String(), "-undo") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
}
