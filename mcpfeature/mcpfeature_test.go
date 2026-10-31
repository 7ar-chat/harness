package mcpfeature_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/engine/tools"
	_ "github.com/7ar-chat/harness/mcpfeature"
)

func TestImportingThePackageGivesTheModelTheToolsOfTheConfiguredServer(t *testing.T) {
	// Arrange: a real MCP server over streamable HTTP, named in MCP_SERVERS
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	server := mcp.NewServer(&mcp.Implementation{Name: "docs", Version: "1"}, nil)
	type args struct {
		Query string `json:"query"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "search", Description: "Search the docs."},
		func(_ context.Context, _ *mcp.CallToolRequest, in args) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "found " + in.Query}}}, nil, nil
		})
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(ts.Close)
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("MCP_SERVERS", `[{"name":"docs","url":"`+ts.URL+`"}]`)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	// Act
	h, err := harness.New(harness.Options{
		Config: cfg, Models: llmtest.Factory{M: llmtest.New()}, Backend: sandboxtest.NewBackend(),
		Features: []harness.Feature{harness.Sandbox(), harness.MCP()},
	})
	if err != nil {
		t.Fatal(err)
	}
	list, cleanup, err := h.Agent.Tools.Tools(context.Background(), tools.RunContext{ThreadID: "t", RunID: "r", Login: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// Assert
	var names []string
	for _, tl := range list {
		info, _ := tl.Info(context.Background())
		names = append(names, info.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "docs__search") {
		t.Fatalf("tools %v, want docs__search from the MCP server", names)
	}
}
