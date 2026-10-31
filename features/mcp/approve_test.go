package mcptools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/approvals"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/mcp"
)

type fakeApprovals struct {
	mode    string
	created []*store.Approval
}

func (f *fakeApprovals) CreateApproval(_ context.Context, a *store.Approval) error {
	a.ID = "ap1"
	f.created = append(f.created, a)
	return nil
}
func (f *fakeApprovals) RunInteractionMode(context.Context, string) (string, error) {
	return f.mode, nil
}

// pendingProvider is a provider whose only server is a project stdio one waiting for approval, with the gate wired as the harness does.
func pendingProvider(t *testing.T, s config.MCPServer, mode string) (*mcptools.Provider, *approvals.Gate, *fakeApprovals) {
	t.Helper()
	fs := &fakeApprovals{mode: mode}
	p := mcptools.NewWithPending(nil, []config.MCPServer{s}, "test", 10000)
	p.TrustDir = t.TempDir()
	p.Ask = true
	g := &approvals.Gate{Store: fs, Describe: p.Describe}
	return p, g, fs
}

func approvalTool(t *testing.T, list []ai.BaseTool) (string, ai.InvokableTool) {
	t.Helper()
	for _, x := range list {
		info, _ := x.Info(context.Background())
		if approvals.IsTrustMCPTool(info.Name) {
			return info.Name, x.(ai.InvokableTool)
		}
	}
	return "", nil
}

func TestPendingProjectServerAsksThenStartsAfterApproval(t *testing.T) {
	// Arrange
	s := projectStdio()
	s.Env["SECRET_TOKEN"] = "hunter2"
	p, g, _ := pendingProvider(t, s, store.InteractionInteractive)
	run := &store.Run{ID: "r", InteractionMode: store.InteractionInteractive}
	ctx := context.Background()
	first, cleanFirst, err := p.Tools(ctx, tools.RunContext{RunID: "r", ThreadID: "t"})
	defer cleanFirst()
	name, approve := approvalTool(t, first)

	// Act
	needs := g.NeedsApproval(ctx, run, name, "{}")
	intr, pauseErr := g.Pause(ctx, run, ai.ToolCall{ID: "c1", Function: ai.FunctionCall{Name: name, Arguments: "{}"}})
	out, runErr := approve.InvokableRun(ctx, "{}") // the resume run executes the approved call
	second, cleanSecond, err2 := p.Tools(ctx, tools.RunContext{RunID: "r2", ThreadID: "t"})
	defer cleanSecond()
	ok, trustErr := config.MCPApproved(p.TrustDir, s.ProjectRoot, s)

	// Assert
	if err != nil || err2 != nil || pauseErr != nil || runErr != nil || trustErr != nil {
		t.Fatal(err, err2, pauseErr, runErr, trustErr)
	}
	if name == "" || len(first) != 1 || !needs {
		t.Fatalf("approval tool %q among %v, needs approval %v", name, names(t, first), needs)
	}
	preview := intr.Approval.ToolInputPreview
	for _, want := range []string{"/repo", `"local"`, `"-test.run=^$"`, "FAKE_MCP_FLAG", "HARNESS_FAKE_MCP", "SECRET_TOKEN"} {
		if !strings.Contains(preview, want) {
			t.Errorf("approval preview lacks %q: %s", want, preview)
		}
	}
	if strings.Contains(preview, "hunter2") || strings.Contains(preview, "flag-ok") {
		t.Errorf("approval preview leaks environment values: %s", preview)
	}
	if !approvals.MatchesCall(intr.Approval, ai.ToolCall{ID: "c1", Function: ai.FunctionCall{Name: name, Arguments: "{}"}}) {
		t.Error("the approved call no longer matches its approval")
	}
	if !ok || !strings.Contains(out, "next run") {
		t.Fatalf("trust recorded %v: %s", ok, out)
	}
	if got := names(t, second); len(got) != 1 || got[0] != "local__echo" {
		t.Fatalf("tools after the approval: %v", got)
	}
}

func TestDeniedProjectServerStaysPendingAndIsNotAskedAgain(t *testing.T) {
	// Arrange
	s := projectStdio()
	p, _, _ := pendingProvider(t, s, store.InteractionInteractive)
	ctx := context.Background()
	first, cleanFirst, _ := p.Tools(ctx, tools.RunContext{RunID: "r", ThreadID: "t"})
	defer cleanFirst()
	name, _ := approvalTool(t, first)
	history := []*store.Message{
		{Role: store.RoleAssistant, ToolCalls: []store.ToolCall{{ID: "c1", Function: store.ToolCallFunction{Name: name, Arguments: "{}"}}}},
	}
	p.History = func(context.Context, string) ([]*store.Message, error) { return history, nil }

	// Act: the question waits without a result, then the user denies (the result is the denial).
	waiting, cleanWaiting, _ := p.Tools(ctx, tools.RunContext{RunID: "r2", ThreadID: "t"})
	defer cleanWaiting()
	history = append(history, &store.Message{Role: store.RoleTool, ToolCallID: "c1", Content: store.ApprovalDeniedResult})
	after, cleanAfter, _ := p.Tools(ctx, tools.RunContext{RunID: "r3", ThreadID: "t"})
	defer cleanAfter()
	trusted, _ := config.MCPApproved(p.TrustDir, s.ProjectRoot, s)

	// Assert
	if n, _ := approvalTool(t, waiting); n != name {
		t.Error("a question still waiting for the user must stay callable")
	}
	if n, _ := approvalTool(t, after); n != "" || len(after) != 0 {
		t.Errorf("a denied server is offered again: %v", names(t, after))
	}
	if trusted {
		t.Error("a denial recorded a trust entry")
	}
}

func TestChangedProjectCommandAsksAgain(t *testing.T) {
	// Arrange: the old command was approved; the project now defines another one.
	s := projectStdio()
	changed := s
	changed.Args = []string{"-test.run=^$", "-test.v"}
	p, g, _ := pendingProvider(t, changed, store.InteractionInteractive)
	if _, err := config.ApproveMCP(p.TrustDir, s.ProjectRoot, s); err != nil {
		t.Fatal(err)
	}
	old := mcptools.NewWithPending(nil, []config.MCPServer{s}, "test", 10000)
	old.TrustDir = p.TrustDir
	old.Ask = true

	// Act
	list, clean, err := p.Tools(context.Background(), tools.RunContext{RunID: "r", ThreadID: "t"})
	defer clean()
	name, _ := approvalTool(t, list)
	oldList, cleanOld, _ := old.Tools(context.Background(), tools.RunContext{RunID: "r", ThreadID: "t"})
	defer cleanOld()

	// Assert
	if err != nil || name == "" || !g.NeedsApproval(context.Background(), &store.Run{ID: "r"}, name, "{}") {
		t.Fatalf("a changed command must ask again: %v %q", err, name)
	}
	if got := names(t, oldList); len(got) != 1 || got[0] != "local__echo" {
		t.Fatalf("the unchanged approved command must start: %v", got)
	}
}

func TestPendingProjectServerFailsClosedWhereNobodyCanAnswer(t *testing.T) {
	// Arrange
	s := projectStdio()
	automatic, gate, _ := pendingProvider(t, s, store.InteractionAutomatic)
	headless := mcptools.NewWithPending(nil, []config.MCPServer{s}, "test", 10000) // no approval gate: Ask stays false
	headless.TrustDir = t.TempDir()
	ctx := context.Background()
	offered, cleanOffered, _ := automatic.Tools(ctx, tools.RunContext{RunID: "r", ThreadID: "t"})
	defer cleanOffered()
	name, _ := approvalTool(t, offered)

	// Act
	denial := gate.Denial(ctx, &store.Run{ID: "r", InteractionMode: store.InteractionAutomatic}, name, "{}")
	needs := gate.NeedsApproval(ctx, &store.Run{ID: "r"}, name, "{}")
	none, cleanNone, err := headless.Tools(ctx, tools.RunContext{RunID: "r", ThreadID: "t"})
	defer cleanNone()
	trusted, _ := config.MCPApproved(automatic.TrustDir, s.ProjectRoot, s)

	// Assert
	if err != nil || len(none) != 0 {
		t.Fatalf("a host without an approval gate must offer nothing: %v %v", err, names(t, none))
	}
	if denial == "" || needs || trusted {
		t.Fatalf("automatic run: denial %q, needs approval %v, trusted %v", denial, needs, trusted)
	}
	if _, err := gate.Pause(ctx, &store.Run{ID: "r"}, ai.ToolCall{ID: "c", Function: ai.FunctionCall{Name: approvals.TrustMCPToolPrefix + "unknown", Arguments: "{}"}}); err == nil {
		t.Error("an approval tool the provider does not know must not be asked")
	}
}
