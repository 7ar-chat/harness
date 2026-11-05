package mcpfeature_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox"
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

func TestAPendingProjectServerIsOfferedForApprovalEvenBehindThePermissionLayer(t *testing.T) {
	// Arrange: the project's .mcp.json defines a local (stdio) server nobody approved yet; the local backend puts the
	// permission layer in front of the approvals gate
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ws, user := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".har"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".mcp.json"), []byte(`{"mcpServers":{"notes":{"command":"notes-server","args":["--stdio"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAR_HOME", user)
	t.Setenv("HARNESS_NO_USER_DIR", "")
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("SANDBOX_BACKEND", "local")
	t.Setenv("LOCAL_WORKSPACE", ws)
	t.Setenv("LOCAL_STATE_DIR", t.TempDir())
	t.Setenv("HARNESS_MCP_STDIO", "1")
	t.Setenv("HARNESS_TRUST_PROJECT", "1")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Layers == nil || len(cfg.Layers.MCPPending) != 1 {
		t.Fatalf("the project's stdio server should be pending approval: %+v", cfg.Layers)
	}

	// Act
	h, err := harness.New(harness.Options{
		Config: cfg, Models: llmtest.Factory{M: llmtest.New()},
		Features: []harness.Feature{harness.Sandbox(), harness.Permissions(false), harness.MCP()}, // the layer wraps the gate before MCP looks for it
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
	var offered string
	for _, tl := range list {
		if info, _ := tl.Info(context.Background()); strings.HasPrefix(info.Name, "mcp_approve__notes_") {
			offered = info.Name
		}
	}
	if offered == "" {
		t.Fatal("the model must be offered the approval tool of the pending server")
	}
}

func TestWithoutServersTheMCPFeatureAddsNoTools(t *testing.T) {
	// Arrange
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("MCP_SERVERS", "")
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
	list, cleanup, _ := h.Agent.Tools.Tools(context.Background(), tools.RunContext{ThreadID: "t", RunID: "r", Login: "alice"})
	defer cleanup()

	// Assert
	for _, tl := range list {
		if info, _ := tl.Info(context.Background()); strings.Contains(info.Name, "__") {
			t.Fatalf("unexpected MCP tool %s", info.Name)
		}
	}
}

func TestADynamicSourceAloneInstallsTheProviderAndIsAskedPerRun(t *testing.T) {
	// Arrange: no configured server, only the source of the marketplace
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("MCP_SERVERS", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	var asked [][2]string
	h, err := harness.New(harness.Options{
		Config: cfg, Models: llmtest.Factory{M: llmtest.New()}, Backend: sandboxtest.NewBackend(),
		Features: []harness.Feature{harness.Sandbox(), harness.MCP()},
		MCPServers: func(_ context.Context, threadID, login string) ([]config.MCPServer, error) {
			asked = append(asked, [2]string{threadID, login})
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	_, cleanup, err := h.Agent.Tools.Tools(context.Background(), tools.RunContext{ThreadID: "th", RunID: "r", Login: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	cleanup()

	// Assert
	if len(asked) != 1 || asked[0] != [2]string{"th", "alice"} {
		t.Fatalf("source asked %v", asked)
	}
}

func TestAServerFromTheDynamicSourceCannotReachALoopbackAddress(t *testing.T) {
	// Arrange: a database-supplied server that points at a service on the host itself
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	hit := false
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	t.Cleanup(ts.Close)
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("MCP_SERVERS", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	h, err := harness.New(harness.Options{
		Config: cfg, Models: llmtest.Factory{M: llmtest.New()}, Backend: sandboxtest.NewBackend(),
		Features: []harness.Feature{harness.Sandbox(), harness.MCP()},
		MCPServers: func(context.Context, string, string) ([]config.MCPServer, error) {
			return []config.MCPServer{{Name: "internal", URL: ts.URL}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	list, cleanup, err := h.Agent.Tools.Tools(context.Background(), tools.RunContext{ThreadID: "th", RunID: "r", Login: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// Assert
	for _, tl := range list {
		if info, _ := tl.Info(context.Background()); strings.HasPrefix(info.Name, "internal__") {
			t.Fatalf("tool %s from a loopback server", info.Name)
		}
	}
	if hit {
		t.Fatal("the loopback server received a request")
	}
}

// browserBox is a sandbox of a fake provider whose "browser" port is a real MCP server.
type browserBox struct {
	sandbox.Sandbox
	url string
}

func (b browserBox) ID() string { return "browser-1" }
func (b browserBox) Exec(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, nil // browser-up started, the ready file exists
}
func (b browserBox) MkdirAll(context.Context, string) error     { return nil }
func (b browserBox) Healthy(context.Context) bool               { return true }
func (b browserBox) Gone(context.Context) bool                  { return false }
func (b browserBox) Renew(context.Context, time.Duration) error { return nil }
func (b browserBox) Endpoint(context.Context, int) (sandbox.Endpoint, error) {
	return sandbox.Endpoint{URL: b.url, Headers: map[string]string{"X-Traffic": "secret"}}, nil
}

type browserBackend struct{ url string }

func (b browserBackend) Create(context.Context, sandbox.CreateOptions) (sandbox.Instance, error) {
	return browserBox{url: b.url}, nil
}
func (browserBackend) Connect(context.Context, string) (sandbox.Instance, error) {
	return nil, sandbox.ErrNoSandbox
}
func (browserBackend) Kill(context.Context, string) error { return nil }

func TestTheBrowserRunsInASandboxOfAnotherProviderThanTheCode(t *testing.T) {
	// Arrange: code sandboxes on one provider (the test backend), the browser on another (a fake whose port is an MCP server)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	server := mcp.NewServer(&mcp.Implementation{Name: "pw", Version: "1"}, nil)
	type nav struct {
		URL string `json:"url"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "browser_navigate", Description: "Go to a page."},
		func(_ context.Context, _ *mcp.CallToolRequest, in nav) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "at " + in.URL}}}, nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "browser_evaluate", Description: "Run code in the page."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ nav) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ran"}}}, nil, nil
		})
	var sawToken bool
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Traffic") == "secret" {
			sawToken = true
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("SANDBOX_BACKEND", "daytona")
	t.Setenv("BROWSER_BACKEND", "e2b")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	// Act
	h, err := harness.New(harness.Options{
		Config: cfg, Models: llmtest.Factory{M: llmtest.New()}, Backend: sandboxtest.NewBackend(),
		BrowserBackend: browserBackend{url: ts.URL},
		Features:       []harness.Feature{harness.Sandbox(), harness.MCP(), harness.Browser()},
	})
	if err != nil {
		t.Fatal(err)
	}
	list, cleanup, err := h.Agent.Tools.Tools(context.Background(), tools.RunContext{ThreadID: "t", RunID: "r", Login: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	defer h.Browser.Close()

	// Assert: the safe tools of the browser are offered, the unsafe one is not, and the provider's token was presented
	var names []string
	for _, tl := range list {
		info, _ := tl.Info(context.Background())
		names = append(names, info.Name)
	}
	all := strings.Join(names, ",")
	if !strings.Contains(all, "browser__browser_navigate") {
		t.Fatalf("tools %v, want the browser's navigate tool", names)
	}
	if strings.Contains(all, "browser_evaluate") {
		t.Fatalf("tools %v: browser_evaluate must not be offered", names)
	}
	if !sawToken {
		t.Error("the MCP requests must carry the provider's access headers")
	}
}
