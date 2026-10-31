package harness_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

func newTestHarness(t *testing.T, turns ...llmtest.Turn) (*harness.Harness, context.Context) {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	h, err := harness.New(harness.Options{
		Config:  cfg,
		Models:  llmtest.Factory{M: llmtest.New(turns...)},
		Backend: sandboxtest.NewBackend(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	go func() { _ = h.Run(ctx) }()
	return h, ctx
}

func TestSendRunsTheAgentInProcessAndStreamsAGUIEvents(t *testing.T) {
	// Arrange
	h, ctx := newTestHarness(t,
		llmtest.Turn{Chunks: llmtest.Text("Hello ", "from the harness")},
		llmtest.Turn{Chunks: llmtest.Text("A title")},
	)
	th, err := h.NewThread(ctx, "alice", "")
	if err != nil {
		t.Fatal(err)
	}

	// Act
	run, err := h.Send(ctx, "alice", th.ID, "Say hello", harness.RunOptions{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	var text strings.Builder
	for ev := range h.Events(ctx, run.ID, 0) {
		types = append(types, ev.Type)
		if ev.Type == agui.TextMessageContent {
			text.WriteString(string(ev.Data))
		}
	}

	// Assert
	if len(types) == 0 || types[0] != agui.RunStarted || types[len(types)-1] != agui.RunFinished {
		t.Fatalf("events %v", types)
	}
	if !strings.Contains(text.String(), "from the harness") {
		t.Fatalf("streamed text %q", text.String())
	}
}

func TestSendRejectsAModelTheConfigDoesNotAllow(t *testing.T) {
	// Arrange
	h, ctx := newTestHarness(t)
	th, _ := h.NewThread(ctx, "alice", "")

	// Act
	_, err := h.Send(ctx, "alice", th.ID, "hi", harness.RunOptions{Model: "nope"})

	// Assert
	if err == nil {
		t.Fatal("expected an error")
	}
}
