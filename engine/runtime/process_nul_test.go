package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
)

func TestProcessPersistsToolOutputContainingNUL(t *testing.T) {
	// Binary tool output (NUL bytes, invalid UTF-8) is sanitized so events and messages are still persisted.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "c1", "execute_command", `{"command":"cat x.bin"}`)}},
		llmtest.Turn{Chunks: llmtest.Text("binary file")},
	)
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		sb.SetExec(func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
			req.OnStdout("a\x00b\xff\n")
			return sandbox.ExecResult{}, nil
		})
	}
	th, run := r.queued("show x.bin")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	if got.Status != store.StatusCompleted || got.LastSeq != int64(len(evs)) || last(evs)["type"] != "RUN_FINISHED" {
		t.Fatalf("status %s lastSeq %d events %v", got.Status, got.LastSeq, types(evs))
	}
	var tool *store.Message
	for _, m := range r.messages(th.ID) {
		if m.Role == store.RoleTool {
			tool = m
		}
	}
	if tool == nil || strings.ContainsRune(tool.Content, 0) || !strings.Contains(tool.Content, "a") {
		t.Fatalf("tool message %+v", tool)
	}
}
