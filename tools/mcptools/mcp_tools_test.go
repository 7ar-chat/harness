package mcptools_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/tools"
	"github.com/GhaziBenDahmane/harness/tools/mcptools"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

type searchIn struct {
	Query string `json:"query"`
}

// kbServer is an MCP server exposing "search" and "admin"; it records the X-Userinfo it receives.
func kbServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "kb", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "search", Description: "Search the knowledge base"},
		func(_ context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "found: " + in.Query}}}, nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "admin", Description: "Dangerous"},
		func(context.Context, *mcp.CallToolRequest, searchIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})
	var mu sync.Mutex
	var seen []string
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("X-Userinfo")+"|"+r.Header.Get("X-Api-Key"))
		mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
}

func names(t *testing.T, ts []tool.BaseTool) []string {
	var out []string
	for _, x := range ts {
		info, _ := x.Info(context.Background())
		out = append(out, info.Name)
	}
	sort.Strings(out)
	return out
}

func TestMCPToolsArePrefixedFilteredAndForwardIdentity(t *testing.T) {
	// Configured MCP tools are exposed as {server}__{tool}, filtered by the allow-list, and called with the user's identity.
	// Arrange
	ts, seen := kbServer(t)
	p := mcptools.New([]config.MCPServer{{Name: "kb", URL: ts.URL, ForwardIdentity: true, Tools: []string{"search"}, Headers: map[string]string{"X-Api-Key": "k"}}}, "test", 10000)

	// Act
	list, cleanup, err := p.Tools(context.Background(), tools.RunContext{RunID: "r", Userinfo: "dXNlcg=="})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	out, callErr := list[0].(tool.InvokableTool).InvokableRun(context.Background(), `{"query":"vacation policy"}`)

	// Assert
	if got := names(t, list); len(got) != 1 || got[0] != "kb__search" {
		t.Fatalf("tools %v", got)
	}
	if callErr != nil || !strings.Contains(out, "found: vacation policy") {
		t.Fatalf("err %v out %s", callErr, out)
	}
	for _, s := range seen() {
		if s != "dXNlcg==|k" {
			t.Fatalf("request headers %q", s)
		}
	}
}

func TestMCPWithoutForwardIdentityDoesNotLeakUserinfo(t *testing.T) {
	// Servers without forwardIdentity never receive X-Userinfo.
	// Arrange
	ts, seen := kbServer(t)
	p := mcptools.New([]config.MCPServer{{Name: "kb", URL: ts.URL}}, "test", 10000)

	// Act
	list, cleanup, err := p.Tools(context.Background(), tools.RunContext{Userinfo: "secret"})
	defer cleanup()

	// Assert
	if err != nil || len(list) != 2 {
		t.Fatalf("err %v tools %d", err, len(list))
	}
	for _, s := range seen() {
		if strings.Contains(s, "secret") {
			t.Fatalf("identity leaked: %q", s)
		}
	}
}

func TestMCPUnreachableServerIsSkipped(t *testing.T) {
	// A failing MCP server is skipped without failing the other servers.
	// Arrange
	ts, _ := kbServer(t)
	p := mcptools.New([]config.MCPServer{{Name: "down", URL: "http://127.0.0.1:1/mcp"}, {Name: "kb", URL: ts.URL, Tools: []string{"search"}}}, "test", 10000)

	// Act
	list, cleanup, err := p.Tools(context.Background(), tools.RunContext{})
	defer cleanup()

	// Assert
	if err != nil || !equalNames(names(t, list), []string{"kb__search"}) {
		t.Fatalf("err %v tools %v", err, names(t, list))
	}
}

func equalNames(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

func TestMCPToolsReportTheirOriginalNamesForApprovals(t *testing.T) {
	// A long server name truncates the exposed name ({server}__{tool}, 64 chars): the tool still reports its original name, so the approval gate sees "delete_everything".
	// Arrange
	srv := mcp.NewServer(&mcp.Implementation{Name: "long", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "delete_everything", Description: "Deletes"},
		func(context.Context, *mcp.CallToolRequest, searchIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(ts.Close)
	server := strings.Repeat("s", 60)
	p := mcptools.New([]config.MCPServer{{Name: server, URL: ts.URL}}, "test", 10000)

	// Act
	list, cleanup, err := p.Tools(context.Background(), tools.RunContext{})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// Assert
	got := names(t, list)
	n, ok := list[0].(tools.ApprovalNamer)
	if len(got) != 1 || got[0] != server+"__de" || !ok {
		t.Fatalf("tools %v namer %v", got, ok)
	}
	if names := n.ApprovalNames(); len(names) != 2 || names[1] != server+"__delete_everything" {
		t.Fatalf("approval names %v", names)
	}
	if _, ok := list[0].(tool.InvokableTool); !ok {
		t.Fatal("not invokable")
	}
}
