package exploretools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/engine/tools/sandboxtools"
	"github.com/GhaziBenDahmane/harness/features/explore"
	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/store/memstore"
)

func TestExploreRunsReadOnlyToolsInItsOwnConversationAndReturnsTheReport(t *testing.T) {
	// Arrange
	be := sandboxtest.NewBackend()
	be.OnCreate = func(s *sandboxtest.Sandbox) { s.Put("/workspace/pkg/a.go", "package pkg\n\nfunc Handle() {}\n") }
	st := memstore.New()
	th, _ := st.CreateThread(context.Background(), "u", "")
	mgr := sandbox.NewManager(be, st, nil, sandbox.ManagerOptions{MaxPerUser: 5})
	fake := llmtest.New(
		llmtest.Turn{Chunks: []*schema.Message{
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
	out, err := ts[0].(tool.InvokableTool).InvokableRun(context.Background(), `{"task":"where is Handle?"}`)

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
		if m.Role == schema.Tool {
			results += m.Content + "\n"
		}
	}
	if !strings.Contains(results, "func Handle()") || !strings.Contains(results, "not available to the explorer") {
		t.Fatalf("tool results %q", results)
	}
}
