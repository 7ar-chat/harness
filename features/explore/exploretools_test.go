package exploretools_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/adapters/store/memstore"
	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/agent"
	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/coding/sandboxtools"
	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/features/explore"
)

func TestExploreRunsReadOnlyToolsInItsOwnConversationAndReturnsTheReport(t *testing.T) {
	// Arrange
	be := sandboxtest.NewBackend()
	be.OnCreate = func(s *sandboxtest.Sandbox) { s.Put("/workspace/pkg/a.go", "package pkg\n\nfunc Handle() {}\n") }
	st := memstore.New()
	th, _ := st.CreateThread(context.Background(), "u", "")
	mgr := sandbox.NewManager(be, st, nil, sandbox.ManagerOptions{MaxPerUser: 5})
	fake := llmtest.New(
		llmtest.Turn{Chunks: []*ai.Message{
			llmtest.Finish(llmtest.ToolCall(0, "c1", "read_file", `{"path":"/workspace/pkg/a.go"}`), "tool_calls"),
			llmtest.Finish(llmtest.ToolCall(1, "c2", "write_file", `{"path":"/workspace/x","content":"no"}`), "tool_calls"),
		}},
		llmtest.Turn{Chunks: llmtest.Text("Handle is in pkg/a.go:3.")},
	)
	p := &exploretools.Provider{Source: sandboxtools.New(mgr, sandboxtools.Options{}), Models: llmtest.Factory{M: fake}, Model: "m"}
	ts, _, err := p.Tools(context.Background(), tools.RunContext{ThreadID: th.ID, Login: "u"})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	out, err := ts[0].(ai.InvokableTool).InvokableRun(context.Background(), `{"task":"where is Handle?"}`)

	// Assert
	if err != nil || out != "Handle is in pkg/a.go:3." {
		t.Fatalf("out %q err %v", out, err)
	}
	calls := fake.Calls()
	if len(calls) != 2 {
		t.Fatalf("%d model calls", len(calls))
	}
	var names []string
	for _, ti := range calls[0].Tools {
		names = append(names, ti.Name)
	}
	if strings.Join(names, ",") != "read_file,list_files,search_files" && len(names) != 3 {
		t.Fatalf("explorer tools %v", names)
	}
	results := ""
	for _, m := range calls[1].Messages {
		if m.Role == ai.RoleTool {
			results += m.Content + "\n"
		}
	}
	if !strings.Contains(results, "func Handle()") || !strings.Contains(results, "not available to the explorer") {
		t.Fatalf("tool results %q", results)
	}
}

func TestAnExplorerCallThatStallsIsCutAndRetriedLikeTheMainLoopsCalls(t *testing.T) {
	// The explorer's model calls run under the same watchdog and retry policy as the main loop's: a provider that goes
	// silent costs one retry, not the rest of the parent run.
	// Arrange
	be := sandboxtest.NewBackend()
	st := memstore.New()
	th, _ := st.CreateThread(context.Background(), "u", "")
	mgr := sandbox.NewManager(be, st, nil, sandbox.ManagerOptions{MaxPerUser: 5})
	fake := llmtest.New(
		llmtest.Turn{Block: true},
		llmtest.Turn{Chunks: llmtest.Text("Nothing relevant found.")},
	)
	p := &exploretools.Provider{
		Source: sandboxtools.New(mgr, sandboxtools.Options{}), Models: llmtest.Factory{M: fake}, Model: "m",
		Caller: &agent.Caller{FirstChunk: 60 * time.Millisecond, Retries: 1, Sleep: func(context.Context, time.Duration) error { return nil }},
	}
	ts, _, err := p.Tools(context.Background(), tools.RunContext{ThreadID: th.ID, Login: "u"})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	start := time.Now()
	out, err := ts[0].(ai.InvokableTool).InvokableRun(context.Background(), `{"task":"find it"}`)

	// Assert
	if err != nil || out != "Nothing relevant found." || len(fake.Calls()) != 2 {
		t.Fatalf("out %q err %v calls %d", out, err, len(fake.Calls()))
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("took %v; the stalled call should be cut after ~60ms", time.Since(start))
	}
}

func TestAnExplorerStepsReadOnlyCallsRunTogetherAndReportInCallOrder(t *testing.T) {
	// Arrange: three independent reads in one step.
	be := sandboxtest.NewBackend()
	be.OnCreate = func(s *sandboxtest.Sandbox) {
		s.Put("/workspace/a.go", "package a\n")
		s.Put("/workspace/b.go", "package b\n")
		s.Put("/workspace/c.go", "package c\n")
	}
	st := memstore.New()
	th, _ := st.CreateThread(context.Background(), "u", "")
	mgr := sandbox.NewManager(be, st, nil, sandbox.ManagerOptions{MaxPerUser: 5})
	fake := llmtest.New(
		llmtest.Turn{Chunks: []*ai.Message{
			llmtest.Finish(llmtest.ToolCall(0, "c1", "read_file", `{"path":"/workspace/a.go"}`), "tool_calls"),
			llmtest.Finish(llmtest.ToolCall(1, "c2", "read_file", `{"path":"/workspace/b.go"}`), "tool_calls"),
			llmtest.Finish(llmtest.ToolCall(2, "c3", "read_file", `{"path":"/workspace/c.go"}`), "tool_calls"),
		}},
		llmtest.Turn{Chunks: llmtest.Text("a, b and c are packages.")},
	)
	p := &exploretools.Provider{Source: sandboxtools.New(mgr, sandboxtools.Options{}), Models: llmtest.Factory{M: fake}, Model: "m"}
	ts, _, _ := p.Tools(context.Background(), tools.RunContext{ThreadID: th.ID, Login: "u"})

	// Act
	out, err := ts[0].(ai.InvokableTool).InvokableRun(context.Background(), `{"task":"what are a, b, c?"}`)

	// Assert
	if err != nil || out != "a, b and c are packages." {
		t.Fatalf("out %q err %v", out, err)
	}
	var ids []string
	for _, m := range fake.Calls()[1].Messages {
		if m.Role == ai.RoleTool {
			ids = append(ids, m.ToolCallID)
		}
	}
	if strings.Join(ids, ",") != "c1,c2,c3" {
		t.Fatalf("tool results %v, want the model's call order", ids)
	}
}
