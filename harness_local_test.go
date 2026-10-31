//go:build unix

package harness_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/core/agui"
)

func TestLocalBackendRunsAgentToolsOnTheHostWorkspace(t *testing.T) {
	// Arrange
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ws := t.TempDir()
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("SANDBOX_BACKEND", "local")
	t.Setenv("LOCAL_WORKSPACE", ws)
	t.Setenv("LOCAL_STATE_DIR", t.TempDir())
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	call := func(id, name, args string) llmtest.Turn {
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, id, name, args), "tool_calls")}}
	}
	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: llmtest.New(
		call("c1", "write_file", `{"path":"/workspace/hello.txt","content":"one\ntwo\n"}`),
		call("c2", "edit_file", `{"path":"/workspace/hello.txt","oldText":"two","newText":"2"}`),
		call("c3", "execute_command", `{"command":"wc -l < /workspace/hello.txt > /workspace/count.txt"}`),
		llmtest.Turn{Chunks: llmtest.Text("done")},
		llmtest.Turn{Chunks: llmtest.Text("A title")},
	)}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() { _ = h.Run(ctx) }()
	th, _ := h.NewThread(ctx, "alice", "")

	// Act
	run, err := h.Send(ctx, "alice", th.ID, "write a file", harness.RunOptions{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	last := ""
	for ev := range h.Events(ctx, run.ID, 0) {
		last = ev.Type
	}

	// Assert
	if last != agui.RunFinished {
		t.Fatalf("run ended with %s", last)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "hello.txt")); string(b) != "one\n2\n" {
		t.Fatalf("hello.txt = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "count.txt")); len(b) == 0 || b[len(b)-2] != '2' {
		t.Fatalf("count.txt = %q", b)
	}
}
