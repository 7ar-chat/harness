package exploretools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/coding/sandboxtools"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/explore"
)

// recorder is an agent emitter that keeps what a tool reports.
type recorder struct {
	events []agui.Event
	usage  [][4]any
}

func (r *recorder) ToolOutput(string, string, string) {}
func (r *recorder) EmitLive(ev agui.Event)            { r.events = append(r.events, ev) }
func (r *recorder) AddSubagentUsage(model string, in, out, cached int) {
	r.usage = append(r.usage, [4]any{model, in, out, cached})
}

func (r *recorder) values() []map[string]any {
	var out []map[string]any
	for _, ev := range r.events {
		if ev["name"] == agui.CustomSubagent {
			out = append(out, ev["value"].(map[string]any))
		}
	}
	return out
}

func exploreTool(t *testing.T, turns ...llmtest.Turn) ai.InvokableTool {
	t.Helper()
	be := sandboxtest.NewBackend()
	be.OnCreate = func(s *sandboxtest.Sandbox) { s.Put("/workspace/pkg/a.go", "package pkg\n\nfunc Handle() {}\n") }
	st := memstore.New()
	th, _ := st.CreateThread(context.Background(), "u", "")
	mgr := sandbox.NewManager(be, st, nil, sandbox.ManagerOptions{MaxPerUser: 5})
	p := &exploretools.Provider{Source: sandboxtools.New(mgr, sandboxtools.Options{}), Models: llmtest.Factory{M: llmtest.New(turns...)}, Model: "cheap"}
	ts, _, err := p.Tools(context.Background(), tools.RunContext{ThreadID: th.ID, Login: "u"})
	if err != nil {
		t.Fatal(err)
	}
	return ts[0].(ai.InvokableTool)
}

func TestExploreReportsItsStepsToolsAndUsageAsSubagentEvents(t *testing.T) {
	// Arrange
	tl := exploreTool(t,
		llmtest.Turn{Chunks: []*ai.Message{
			llmtest.WithUsage(llmtest.Finish(llmtest.ToolCall(0, "c1", "read_file", `{"path":"/workspace/pkg/a.go","offset":2,"limit":5}`), "tool_calls"), 100, 10),
			llmtest.Finish(llmtest.ToolCall(1, "c2", "search_files", `{"pattern":"Handle"}`), "tool_calls"),
		}},
		llmtest.Turn{Chunks: []*ai.Message{llmtest.WithUsage(llmtest.Finish(&ai.Message{Role: ai.RoleAssistant, Content: "Handle is in pkg/a.go:3."}, "stop"), 150, 20)}},
	)
	rec := &recorder{}
	ctx := tools.WithCall(context.Background(), "call_9", rec)

	// Act
	out, err := tl.InvokableRun(ctx, `{"task":"where is Handle?"}`)

	// Assert
	if err != nil || out != "Handle is in pkg/a.go:3." {
		t.Fatalf("out %q err %v", out, err)
	}
	var phases []string
	var toolArgs []string
	for _, v := range rec.values() {
		if v["toolCallId"] != "call_9" || v["agent"] != "explore" {
			t.Fatalf("event %v", v)
		}
		phases = append(phases, v["phase"].(string))
		if v["phase"] == agui.SubagentTool {
			toolArgs = append(toolArgs, v["tool"].(map[string]any)["name"].(string)+" "+v["tool"].(map[string]any)["args"].(string))
		}
	}
	if strings.Join(phases, ",") != "start,step,tool,tool,step,end" {
		t.Fatalf("phases %v", phases)
	}
	if strings.Join(toolArgs, "|") != `read_file /workspace/pkg/a.go:2-6|search_files "Handle"` {
		t.Fatalf("tools %v", toolArgs)
	}
	vs := rec.values()
	if vs[0]["task"] != "where is Handle?" || vs[len(vs)-1]["reportChars"] != len(out) {
		t.Fatalf("start %v end %v", vs[0], vs[len(vs)-1])
	}
	if len(rec.usage) != 1 || rec.usage[0] != [4]any{"cheap", 250, 30, 0} {
		t.Fatalf("usage %v", rec.usage)
	}
}

func TestExploreWithoutAnEmitterRunsSilently(t *testing.T) {
	// Arrange
	tl := exploreTool(t, llmtest.Turn{Chunks: llmtest.Text("nothing")})

	// Act
	out, err := tl.InvokableRun(context.Background(), `{"task":"x"}`)

	// Assert
	if err != nil || out != "nothing" {
		t.Fatalf("out %q err %v", out, err)
	}
}

func TestExploreCapsTheNumberOfSubagentEvents(t *testing.T) {
	// Arrange: 11 steps of 20 tool calls each
	var calls []*ai.Message
	for i := 0; i < 20; i++ {
		calls = append(calls, llmtest.Finish(llmtest.ToolCall(i, "c"+string(rune('a'+i)), "list_files", `{"path":"/workspace"}`), "tool_calls"))
	}
	var turns []llmtest.Turn
	for i := 0; i < 11; i++ {
		turns = append(turns, llmtest.Turn{Chunks: calls})
	}
	turns = append(turns, llmtest.Turn{Chunks: llmtest.Text("done")})
	tl := exploreTool(t, turns...)
	rec := &recorder{}

	// Act
	_, err := tl.InvokableRun(tools.WithCall(context.Background(), "c", rec), `{"task":"x"}`)

	// Assert
	vs := rec.values()
	if err != nil || len(vs) != 200 || vs[len(vs)-1]["phase"] != agui.SubagentEnd {
		t.Fatalf("%d events, err %v", len(vs), err)
	}
}
