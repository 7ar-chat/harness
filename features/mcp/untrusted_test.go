package mcptools

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/engine/tools"
)

type noIn struct{}

// annotatedServer offers "lookup" (readOnlyHint), "delete" (destructive, no hint) and "plain" (no annotations).
func annotatedServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "ann", Version: "1"}, nil)
	h := func(context.Context, *mcp.CallToolRequest, noIn) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "lookup", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, h)
	mcp.AddTool(srv, &mcp.Tool{Name: "delete", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false}}, h)
	mcp.AddTool(srv, &mcp.Tool{Name: "plain"}, h)
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(ts.Close)
	return ts
}

func runTools(t *testing.T, p *Provider, rc tools.RunContext) []string {
	t.Helper()
	list, cleanup, err := p.Tools(context.Background(), rc)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	var out []string
	for _, x := range list {
		info, _ := x.(ai.BaseTool).Info(context.Background())
		out = append(out, info.Name)
	}
	sort.Strings(out)
	return out
}

// loopbackProvider is a provider whose untrusted servers may reach the httptest servers on loopback: the only
// seam that weakens the guard, set by tests alone.
func loopbackProvider(dyn func(context.Context, string, string) ([]config.MCPServer, error), static ...config.MCPServer) *Provider {
	p := NewDynamic(dyn, "test", 10000)
	p.servers = static
	p.transport = http.DefaultTransport
	return p
}

func dynamic(servers ...config.MCPServer) func(context.Context, string, string) ([]config.MCPServer, error) {
	return func(context.Context, string, string) ([]config.MCPServer, error) { return servers, nil }
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAnUntrustedServerWithoutAnAllowListExposesOnlyReadOnlyTools(t *testing.T) {
	ts := annotatedServer(t)
	p := loopbackProvider(dynamic(config.MCPServer{Name: "ann", URL: ts.URL}))

	got := runTools(t, p, tools.RunContext{RunID: "r"})

	if want := []string{"ann__lookup"}; !equal(got, want) {
		t.Fatalf("tools %v, want %v", got, want)
	}
}

func TestAnUntrustedServerWithAnAllowListExposesExactlyThoseTools(t *testing.T) {
	ts := annotatedServer(t)
	p := loopbackProvider(dynamic(config.MCPServer{Name: "ann", URL: ts.URL, Tools: []string{"delete", "plain"}}))

	got := runTools(t, p, tools.RunContext{RunID: "r"})

	if want := []string{"ann__delete", "ann__plain"}; !equal(got, want) {
		t.Fatalf("tools %v, want %v", got, want)
	}
}

func TestAStaticServerKeepsExposingEveryTool(t *testing.T) {
	ts := annotatedServer(t)
	p := loopbackProvider(nil, config.MCPServer{Name: "ann", URL: ts.URL})

	got := runTools(t, p, tools.RunContext{RunID: "r"})

	if len(got) != 3 {
		t.Fatalf("tools %v: the configured server's tools are the operator's choice", got)
	}
}

func TestAnUntrustedStdioServerIsRefused(t *testing.T) {
	p := loopbackProvider(dynamic(config.MCPServer{Name: "evil", Command: "/bin/sh", Args: []string{"-c", "touch " + t.TempDir() + "/pwned"}}))

	got := runTools(t, p, tools.RunContext{RunID: "r"})

	if len(got) != 0 {
		t.Fatalf("tools %v", got)
	}
	_, _, err := p.connect(context.Background(), config.MCPServer{Name: "evil", Command: "true", Untrusted: true}, tools.RunContext{}, map[string]bool{})
	if !errors.Is(err, errUntrustedStdio) {
		t.Fatalf("error %v", err)
	}
	if _, err := ListTools(context.Background(), config.MCPServer{Name: "evil", Command: "true"}, "t", 1e9); !errors.Is(err, errUntrustedStdio) {
		t.Fatalf("ListTools error %v", err)
	}
}

func TestADynamicSourceThatFailsKeepsTheStaticTools(t *testing.T) {
	ts := annotatedServer(t)
	p := loopbackProvider(func(context.Context, string, string) ([]config.MCPServer, error) {
		return []config.MCPServer{{Name: "ghost", URL: "https://never.example/"}}, errors.New("database down")
	}, config.MCPServer{Name: "ann", URL: ts.URL, Tools: []string{"plain"}})

	got := runTools(t, p, tools.RunContext{RunID: "r"})

	if want := []string{"ann__plain"}; !equal(got, want) {
		t.Fatalf("tools %v, want %v", got, want)
	}
}

func TestADynamicServerCannotShadowAStaticOne(t *testing.T) {
	ts := annotatedServer(t)
	p := loopbackProvider(dynamic(config.MCPServer{Name: "ann", URL: ts.URL, Tools: []string{"delete"}}),
		config.MCPServer{Name: "ann", URL: ts.URL, Tools: []string{"plain"}})

	got := runTools(t, p, tools.RunContext{RunID: "r"})

	if want := []string{"ann__plain"}; !equal(got, want) {
		t.Fatalf("tools %v, want %v", got, want)
	}
}

func TestTheDynamicSourceReceivesTheRunsThreadAndLogin(t *testing.T) {
	var thread, login string
	p := loopbackProvider(func(_ context.Context, th, lg string) ([]config.MCPServer, error) {
		thread, login = th, lg
		return nil, nil
	})

	runTools(t, p, tools.RunContext{RunID: "r", ThreadID: "th1", Login: "alice"})

	if thread != "th1" || login != "alice" {
		t.Fatalf("source called with %q %q", thread, login)
	}
}

func TestUntrustedServersDialThroughTheGuardByDefault(t *testing.T) {
	ts := annotatedServer(t)
	p := NewDynamic(dynamic(config.MCPServer{Name: "ann", URL: ts.URL, Tools: []string{"plain"}}), "test", 10000)

	got := runTools(t, p, tools.RunContext{RunID: "r"})

	if len(got) != 0 {
		t.Fatalf("a loopback server was reached through the production transport: %v", got)
	}
}

func TestListToolsListsEverythingAndIgnoresTheAllowList(t *testing.T) {
	ts := annotatedServer(t)

	got, err := listTools(context.Background(), config.MCPServer{Name: "ann", URL: ts.URL, Tools: []string{"plain"}}, "t", 5e9, http.DefaultTransport)

	if err != nil {
		t.Fatal(err)
	}
	ro := map[string]bool{}
	for _, ti := range got {
		ro[ti.Name] = ti.ReadOnly
	}
	if len(ro) != 3 || !ro["lookup"] || ro["delete"] || ro["plain"] {
		t.Fatalf("tools %v", got)
	}
}

func TestListToolsThroughTheProductionTransportRefusesLoopback(t *testing.T) {
	ts := annotatedServer(t)

	if _, err := ListTools(context.Background(), config.MCPServer{Name: "ann", URL: ts.URL}, "t", 5e9); err == nil {
		t.Fatal("ListTools reached a loopback server")
	}
}
