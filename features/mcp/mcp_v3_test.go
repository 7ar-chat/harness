package mcptools_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/features/mcp"
)

// v3Server exposes tools whose names collide after sanitizing/truncation, a slow tool and a read-only one.
func v3Server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "jira", Version: "1"}, nil)
	echo := func(text string) mcp.ToolHandlerFor[searchIn, any] {
		return func(context.Context, *mcp.CallToolRequest, searchIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
		}
	}
	long := strings.Repeat("x", 60)
	mcp.AddTool(srv, &mcp.Tool{Name: "issue.get", Description: "dotted"}, echo("dotted"))
	mcp.AddTool(srv, &mcp.Tool{Name: "issue_get", Description: "underscored"}, echo("underscored"))
	mcp.AddTool(srv, &mcp.Tool{Name: long + "_list", Description: "list"}, echo("list"))
	mcp.AddTool(srv, &mcp.Tool{Name: long + "_delete", Description: "delete"}, echo("delete"))
	mcp.AddTool(srv, &mcp.Tool{Name: "search", Description: "read only", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, echo("found"))
	mcp.AddTool(srv, &mcp.Tool{Name: "slow", Description: "slow"},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ searchIn) (*mcp.CallToolResult, any, error) {
			select {
			case <-ctx.Done():
			case <-time.After(1500 * time.Millisecond):
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "late"}}}, nil, nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}

func v3Tools(t *testing.T, callTimeout time.Duration) map[string]tool.BaseTool {
	t.Helper()
	ts := v3Server(t)
	p := mcptools.New([]config.MCPServer{{Name: "jira", URL: ts.URL}}, "test", 10000)
	p.CallTimeout = callTimeout
	list, cleanup, err := p.Tools(context.Background(), tools.RunContext{RunID: "r"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	out := map[string]tool.BaseTool{}
	for _, x := range list {
		info, _ := x.Info(context.Background())
		out[info.Name] = x
	}
	return out
}

func TestMCPNameCollisionsGetASuffixInsteadOfDroppingATool(t *testing.T) {
	// Tools whose sanitized or 64-char-truncated names collide are all exposed, each under a unique name (hash suffix) that calls its own tool with its own approval names (A-23).
	// Arrange
	byName := v3Tools(t, 0)

	// Act
	results := map[string]string{}
	for name, x := range byName {
		if len(name) > 64 {
			t.Fatalf("name %q longer than 64", name)
		}
		if name == "jira__slow" {
			continue
		}
		if out, err := x.(tool.InvokableTool).InvokableRun(context.Background(), `{"query":"q"}`); err == nil {
			results[name] = out
		}
	}

	// Assert
	texts := map[string]bool{}
	for _, out := range results {
		for _, want := range []string{"dotted", "underscored", "list", "delete", "found"} {
			if strings.Contains(out, want) {
				texts[want] = true
			}
		}
	}
	if len(byName) != 6 || len(texts) != 5 {
		t.Fatalf("tools %d, distinct results %v", len(byName), texts)
	}
	for name, x := range byName {
		names := x.(tools.ApprovalNamer).ApprovalNames()
		if len(names) != 2 || names[0] != name {
			t.Fatalf("%s approval names %v", name, names)
		}
	}
}

func TestMCPCallsTimeOut(t *testing.T) {
	// A slow MCP tool call fails with a timeout error after MCP_CALL_TIMEOUT_SECONDS instead of hanging the run.
	// Arrange
	slow := v3Tools(t, 200*time.Millisecond)["jira__slow"].(tool.InvokableTool)

	// Act
	start := time.Now()
	_, err := slow.InvokableRun(context.Background(), `{"query":"q"}`)

	// Assert
	if err == nil || time.Since(start) > 2*time.Second || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

func TestMCPReadOnlyHintMakesAToolReadOnly(t *testing.T) {
	// Tools annotated readOnlyHint implement tools.ReadOnly (they may run in parallel); others do not.
	// Arrange
	byName := v3Tools(t, 0)

	// Act
	search, _ := byName["jira__search"].(tools.ReadOnly)
	get, _ := byName["jira__issue_get"].(tools.ReadOnly)

	// Assert
	if search == nil || !search.ReadOnly() || (get != nil && get.ReadOnly()) {
		t.Fatalf("search %v get %v", search, get)
	}
}
