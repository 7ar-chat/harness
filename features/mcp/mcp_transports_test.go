package mcptools_test

import (
	"context"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/features/mcp"
)

// fakeServer exposes one tool, "echo".
func fakeServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: "Echo the query"},
		func(_ context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + in.Query + " " + os.Getenv("FAKE_MCP_FLAG")}}}, nil, nil
		})
	return srv
}

// runFakeStdio serves the fake server on stdin/stdout when the test binary is started as a stdio MCP server.
func runFakeStdio() {
	_ = fakeServer().Run(context.Background(), &mcp.StdioTransport{})
}

func callEcho(t *testing.T, ts []ai.BaseTool) string {
	t.Helper()
	if len(ts) != 1 {
		t.Fatalf("tools %v", names(t, ts))
	}
	out, err := ts[0].(ai.InvokableTool).InvokableRun(context.Background(), `{"query":"hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestInMemoryTransportServer(t *testing.T) {
	// Arrange
	ct, st := mcp.NewInMemoryTransports()
	go func() { _ = fakeServer().Run(context.Background(), st) }()

	// Act
	ts, sess, err := mcptools.Connect(context.Background(), mcptools.Server{Name: "mem", ClientTransport: ct}, "test", 5*time.Second, 10000)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// Assert
	if got := names(t, ts); len(got) != 1 || got[0] != "mem__echo" {
		t.Fatalf("tools %v", got)
	}
	if out := callEcho(t, ts); !strings.Contains(out, "echo: hi") {
		t.Fatalf("out %s", out)
	}
}

func TestSSEServer(t *testing.T) {
	// Arrange
	h := mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return fakeServer() }, nil)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	p := mcptools.New([]config.MCPServer{{Name: "s", URL: ts.URL, Type: "sse"}}, "test", 10000)

	// Act
	list, cleanup, err := p.Tools(context.Background(), tools.RunContext{RunID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// Assert
	if out := callEcho(t, list); !strings.Contains(out, "echo: hi") {
		t.Fatalf("out %s", out)
	}
}

func TestStdioServerIsAChildProcessWithAReducedEnvironment(t *testing.T) {
	// Arrange: the test binary itself is the server (see TestMain).
	t.Setenv("LLM_API_KEY", "must-not-leak")
	p := mcptools.New([]config.MCPServer{{Name: "local", Command: os.Args[0], Args: []string{"-test.run=^$"},
		Env: map[string]string{"HARNESS_FAKE_MCP": "1", "FAKE_MCP_FLAG": "flag-ok"}}}, "test", 10000)

	// Act
	list, cleanup, err := p.Tools(context.Background(), tools.RunContext{RunID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// Assert
	out := callEcho(t, list)
	if !strings.Contains(out, "echo: hi flag-ok") {
		t.Fatalf("out %s", out)
	}
	if strings.Contains(out, "must-not-leak") {
		t.Fatalf("the child inherited the harness secrets: %s", out)
	}
}

func projectStdio() config.MCPServer {
	return config.MCPServer{Name: "local", Command: os.Args[0], Args: []string{"-test.run=^$"}, ProjectRoot: "/repo",
		Env: map[string]string{"HARNESS_FAKE_MCP": "1", "FAKE_MCP_FLAG": "flag-ok"}}
}

func TestAProjectStdioServerIsOnlySpawnedWithTheApprovalOfItsExactCommand(t *testing.T) {
	// Arrange
	trust := t.TempDir()
	s := projectStdio()
	newProvider := func(s config.MCPServer) *mcptools.Provider {
		p := mcptools.New([]config.MCPServer{s}, "test", 10000)
		p.TrustDir = trust
		return p
	}

	// Act: denied (no approval), granted, then the command changed after the approval.
	denied, cleanDenied, err1 := newProvider(s).Tools(context.Background(), tools.RunContext{RunID: "r"})
	defer cleanDenied()
	if _, err := config.ApproveMCP(trust, "/repo", s); err != nil {
		t.Fatal(err)
	}
	granted, cleanGranted, err2 := newProvider(s).Tools(context.Background(), tools.RunContext{RunID: "r"})
	defer cleanGranted()
	changed := s
	changed.Args = []string{"-test.run=^$", "-test.v"}
	moved, cleanMoved, err3 := newProvider(changed).Tools(context.Background(), tools.RunContext{RunID: "r"})
	defer cleanMoved()

	// Assert
	if err1 != nil || err2 != nil || err3 != nil {
		t.Fatal(err1, err2, err3)
	}
	if len(denied) != 0 {
		t.Fatalf("an unapproved project server was spawned: %v", names(t, denied))
	}
	if out := callEcho(t, granted); !strings.Contains(out, "echo: hi flag-ok") {
		t.Fatalf("approved server: %s", out)
	}
	if len(moved) != 0 {
		t.Fatalf("a changed command kept its approval: %v", names(t, moved))
	}
}

func TestAUserStdioServerNeedsNoProjectApproval(t *testing.T) {
	// Arrange: the same server without a ProjectRoot (the user's own mcp.json) and no trust file at all.
	s := projectStdio()
	s.ProjectRoot = ""
	p := mcptools.New([]config.MCPServer{s}, "test", 10000)
	p.TrustDir = t.TempDir()

	// Act
	list, cleanup, err := p.Tools(context.Background(), tools.RunContext{RunID: "r"})
	defer cleanup()

	// Assert
	if err != nil || len(list) != 1 {
		t.Fatalf("%v %v", err, list)
	}
}
