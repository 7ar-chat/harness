package harness_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/longtest"
)

type modeRun struct {
	h      *harness.Harness
	model  *llmtest.Model
	run    *store.Run
	ctx    context.Context
	pinged bool
}

// runWith builds a work-profile harness with a custom tool "ping" and plays
// one run whose model calls first, then answers.
func runWith(t *testing.T, backend *sandboxtest.Backend, mode harness.SandboxMode, first *ai.Message) *modeRun {
	t.Helper()
	m := &modeRun{}
	opts := harness.Options{Config: featureConfig(t, nil)}
	if backend != nil {
		opts.Backend = backend
	}
	opts.Tools = append(opts.Tools, longtest.ExtraTool("ping", func(context.Context, string) (string, error) { m.pinged = true; return "pong", nil }))
	m.model = llmtest.New(
		llmtest.Turn{Chunks: []*ai.Message{first}},
		llmtest.Turn{Chunks: llmtest.Text("all good")},
		llmtest.Turn{Chunks: llmtest.Text("A title")},
	)
	opts.Models = llmtest.Factory{M: m.model}
	h, err := harness.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	go func() { _ = h.Run(ctx) }()
	th, _ := h.NewThread(ctx, "alice", "")
	run, err := h.Send(ctx, "alice", th.ID, "do it", harness.RunOptions{Model: "gpt-test", Sandbox: mode})
	if err != nil {
		t.Fatal(err)
	}
	var last string
	for ev := range h.Events(ctx, run.ID, 0) {
		last = ev.Type
	}
	if last != agui.RunFinished {
		t.Fatalf("last event %q", last)
	}
	m.h, m.ctx = h, ctx
	m.run, _ = h.Store.GetRun(ctx, run.ID)
	return m
}

func (m *modeRun) firstRequest() (system string, tools []string) {
	c := m.model.Calls()[0]
	for _, msg := range c.Messages {
		system += msg.Content + "\n"
	}
	for _, ti := range c.Tools {
		tools = append(tools, ti.Name)
	}
	return
}

var pingCall = llmtest.ToolCall(0, "c1", "ping", "{}")

func TestOffRunUsesCustomToolsOnlyAndNeverCreatesASandbox(t *testing.T) {
	// Arrange
	be := sandboxtest.NewBackend()

	// Act
	m := runWith(t, be, harness.SandboxOff, pingCall)

	// Assert
	_, tools := m.firstRequest()
	if !m.pinged || !contains(tools, "ping") {
		t.Fatalf("custom tool missing or unused: %v pinged=%v", tools, m.pinged)
	}
	for _, name := range []string{"execute_command", "read_file", "write_file", "edit_file", "list_files", "search_files"} {
		if contains(tools, name) {
			t.Fatalf("off run has sandbox tool %s: %v", name, tools)
		}
	}
	if n := len(be.Created()); n != 0 {
		t.Fatalf("Backend.Create called %d times", n)
	}
	if m.run.Sandbox != store.SandboxOff {
		t.Fatalf("run record sandbox = %q", m.run.Sandbox)
	}
}

func TestAutoRunThatNeverNeedsACommandNeverCreatesASandbox(t *testing.T) {
	// Arrange
	be := sandboxtest.NewBackend()

	// Act
	m := runWith(t, be, "", pingCall)

	// Assert
	_, tools := m.firstRequest()
	if !contains(tools, "execute_command") || !contains(tools, "ping") {
		t.Fatalf("auto run lacks tools: %v", tools)
	}
	if n := len(be.Created()); n != 0 {
		t.Fatalf("Backend.Create called %d times for a run without commands", n)
	}
	if m.run.Sandbox != store.SandboxAuto {
		t.Fatalf("run record sandbox = %q", m.run.Sandbox)
	}
}

func TestAutoRunCreatesTheSandboxOnFirstCommand(t *testing.T) {
	// Arrange
	be := sandboxtest.NewBackend()

	// Act
	runWith(t, be, harness.SandboxAuto, llmtest.ToolCall(0, "c1", "execute_command", `{"command":"echo hi"}`))

	// Assert
	if n := len(be.Created()); n != 1 {
		t.Fatalf("Backend.Create called %d times, want 1", n)
	}
}

func TestOnWithoutABackendFailsTheSend(t *testing.T) {
	// Arrange
	cfg := featureConfig(t, nil)
	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: llmtest.New()}})
	if err != nil {
		t.Fatal(err)
	}
	th, _ := h.NewThread(context.Background(), "alice", "")

	// Act
	_, err = h.Send(context.Background(), "alice", th.ID, "x", harness.RunOptions{Model: "gpt-test", Sandbox: harness.SandboxOn})
	_, bad := h.Send(context.Background(), "alice", th.ID, "x", harness.RunOptions{Model: "gpt-test", Sandbox: "maybe"})

	// Assert
	if !errors.Is(err, harness.ErrInvalidRequest) || !errors.Is(err, harness.ErrNoSandbox) {
		t.Fatalf("on without backend: %v", err)
	}
	if !errors.Is(bad, harness.ErrInvalidRequest) {
		t.Fatalf("unknown mode: %v", bad)
	}
	if h.HasSandbox() {
		t.Fatal("HasSandbox with no backend")
	}
}

func TestOnWithABackendIsAccepted(t *testing.T) {
	// Act
	m := runWith(t, sandboxtest.NewBackend(), harness.SandboxOn, pingCall)

	// Assert
	if m.run.Sandbox != store.SandboxOn {
		t.Fatalf("run record sandbox = %q", m.run.Sandbox)
	}
}

func TestThePromptDiffersBetweenOffAndAuto(t *testing.T) {
	// Act
	off, offTools := runWith(t, sandboxtest.NewBackend(), harness.SandboxOff, pingCall).firstRequest()
	auto, autoTools := runWith(t, sandboxtest.NewBackend(), harness.SandboxAuto, pingCall).firstRequest()

	// Assert
	if !strings.Contains(off, "No sandbox is available") || strings.Contains(off, "private Linux sandbox") {
		t.Fatalf("off prompt should be the no-sandbox one:\n%s", off)
	}
	if strings.Contains(auto, "No sandbox is available") || !strings.Contains(auto, "private Linux sandbox") {
		t.Fatalf("auto prompt should describe the sandbox:\n%s", auto)
	}
	if strings.Contains(off, "Local Git Checkpoints") {
		t.Fatal("the workspace checkpoint section leaked into an off run")
	}
	if len(offTools) >= len(autoTools) {
		t.Fatalf("off has %d tools, auto %d", len(offTools), len(autoTools))
	}
}

func TestAHarnessWithoutABackendRunsWithCustomTools(t *testing.T) {
	// Act
	m := runWith(t, nil, "", pingCall)

	// Assert
	system, tools := m.firstRequest()
	if !m.pinged {
		t.Fatalf("custom tool did not run: %v", tools)
	}
	if contains(tools, "execute_command") || !strings.Contains(system, "No sandbox is available") {
		t.Fatalf("a harness without backend must use the no-sandbox prompt and tools: %v", tools)
	}
}

func TestResumeAndFollowUpRunsKeepTheSandboxMode(t *testing.T) {
	// Arrange
	m := runWith(t, sandboxtest.NewBackend(), harness.SandboxOff, pingCall)

	// Act: a run created without a mode (resume, goal turn, compaction)
	next, _, err := m.h.Store.CreateRun(m.ctx, store.NewRun{RunID: "00000000-0000-4000-8000-000000000001", ThreadID: m.run.ThreadID,
		UserLogin: "alice", Model: "gpt-test", Message: store.Message{Role: store.RoleUser, Content: "again"}})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if next.Sandbox != store.SandboxOff {
		t.Fatalf("follow-up run sandbox = %q, want off", next.Sandbox)
	}
}
