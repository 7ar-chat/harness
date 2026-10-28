package mcptools_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

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

func callEcho(t *testing.T, ts []tool.BaseTool) string {
	t.Helper()
	if len(ts) != 1 {
		t.Fatalf("tools %v", names(t, ts))
	}
	out, err := ts[0].(tool.InvokableTool).InvokableRun(context.Background(), `{"query":"hi"}`)
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
