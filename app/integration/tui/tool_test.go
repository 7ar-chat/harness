package tui_integration_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/hosts/inproc"
	"github.com/7ar-chat/harness/hosts/tui"
)

// toolRig is a backend plus the harness behind it, so a test can look into the sandbox.
type toolRig struct {
	h   *harness.Harness
	be  *tui.ClientBackend
	ctx context.Context
	tid string
}

// newToolRig starts a harness (the in-memory sandbox unless local) behind a ClientBackend.
// The environment of the config is set by the caller before.
func newToolRig(t *testing.T, interactive, local bool, model *llmtest.Model) *toolRig {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("LLM_STEP_RETRIES", "0")
	t.Setenv("AGENT_PERMISSIONS", "")
	t.Setenv("AGENT_PERMISSIONS_DENY", "")
	opts := harness.Options{Models: llmtest.Factory{M: model}}
	if local {
		t.Setenv("SANDBOX_BACKEND", "local")
		t.Setenv("LOCAL_WORKSPACE", t.TempDir())
		t.Setenv("LOCAL_STATE_DIR", t.TempDir())
	} else {
		opts.Backend = sandboxtest.NewBackend()
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	opts.Config = cfg
	h, err := harness.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	go func() { _ = h.Run(ctx) }()
	th, err := h.NewThread(ctx, "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	be := &tui.ClientBackend{C: inproc.New(h, "alice", inproc.WithModels(cfg.LLMModels, "gpt-test")), ThreadID: th.ID, ModelID: "gpt-test", Interactive: interactive}
	return &toolRig{h: h, be: be, ctx: ctx, tid: th.ID}
}

// scriptedTool asks for one tool call, then answers final once it has seen the tool result;
// calls without tools (the title) get "A title".
func scriptedTool(tool, args, final string) *llmtest.Model {
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		if c.Messages[len(c.Messages)-1].Role == ai.RoleTool {
			return llmtest.Turn{Chunks: llmtest.Text(final)}
		}
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", tool, args), "tool_calls")}}
	})
}

// drain decodes every event of a run until its stream closes.
func (r *toolRig) drain(runID string) []tui.Ev {
	var evs []tui.Ev
	for se := range r.be.Events(r.ctx, runID) {
		evs = append(evs, tui.Decode(se))
	}
	return evs
}

func TestPrintModeWriteFileToolLeavesTheFileInTheSandbox(t *testing.T) {
	// Arrange
	r := newToolRig(t, false, false, scriptedTool("write_file", `{"path":"notes/out.md","content":"# hello\n"}`, "Wrote it"))
	var stdout, stderr bytes.Buffer

	// Act
	code := tui.RunPrint(r.ctx, r.be, "write the notes", tui.PrintOptions{Stdout: &stdout, Stderr: &stderr})

	// Assert
	if code != tui.ExitOK {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if stdout.String() != "Wrote it\n" {
		t.Fatalf("stdout %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "> write_file notes/out.md\n") || !strings.Contains(stderr.String(), "  ok:") {
		t.Fatalf("stderr %q", stderr.String())
	}
	sb, err := r.h.Sandboxes.Acquire(r.ctx, r.tid, "alice")
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := sb.ReadFile(r.ctx, "/workspace/notes/out.md", 1<<20)
	if err != nil {
		t.Fatalf("the file is not in the sandbox: %v", err)
	}
	if string(data) != "# hello\n" {
		t.Fatalf("file content %q", data)
	}
}

func TestToolRunDecodesToTheTimelineEventsInOrder(t *testing.T) {
	// Arrange
	r := newToolRig(t, false, false, scriptedTool("write_file", `{"path":"a.txt","content":"A"}`, "All done"))
	runID, err := r.be.Send(r.ctx, "write a.txt")
	if err != nil {
		t.Fatal(err)
	}

	// Act
	evs := r.drain(runID)

	// Assert: the tool call is announced, its arguments stream, it ends, its result
	// arrives, then the answer streams, and the run finishes.
	var order []string
	var args, answer string
	for _, ev := range evs {
		switch ev.Type {
		case agui.ToolCallStart, agui.ToolCallEnd, agui.ToolCallResult, agui.TextMessageContent, agui.RunStarted, agui.RunFinished:
			if n := len(order); n == 0 || order[n-1] != ev.Type {
				order = append(order, ev.Type)
			}
		case agui.ToolCallArgs:
			args += ev.Delta
			if n := len(order); n == 0 || order[n-1] != ev.Type {
				order = append(order, ev.Type)
			}
		}
		if ev.Type == agui.TextMessageContent {
			answer += ev.Delta
		}
		if ev.Type == agui.ToolCallStart && (ev.ToolName != "write_file" || ev.ToolCallID != "c1") {
			t.Fatalf("tool start %+v", ev)
		}
	}
	want := []string{agui.RunStarted, agui.ToolCallStart, agui.ToolCallArgs, agui.ToolCallEnd, agui.ToolCallResult, agui.TextMessageContent, agui.RunFinished}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order %v, want %v", order, want)
	}
	if args != `{"path":"a.txt","content":"A"}` {
		t.Fatalf("streamed args %q", args)
	}
	if answer != "All done" {
		t.Fatalf("answer %q", answer)
	}
	for _, ev := range evs {
		if ev.Type == agui.ToolCallResult && (ev.ToolCallID != "c1" || ev.IsError) {
			t.Fatalf("tool result %+v", ev)
		}
	}
}
