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

// kbServer is an MCP server exposing "search" and "admin"; it records the X-Api-Key it receives.
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
		seen = append(seen, r.Header.Get("X-Api-Key"))
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

func TestMCPToolsArePrefixedAndFiltered(t *testing.T) {
	// Configured MCP tools are exposed as {server}__{tool}, filtered by the allow-list.
	// Arrange
	ts, seen := kbServer(t)
	p := mcptools.New([]config.MCPServer{{Name: "kb", URL: ts.URL, Tools: []string{"search"}, Headers: map[string]string{"X-Api-Key": "k"}}}, "test", 10000)

	// Act
	list, cleanup, err := p.Tools(context.Background(), tools.RunContext{RunID: "r"})
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
		if s != "k" {
			t.Fatalf("request headers %q", s)
		}
	}
}
