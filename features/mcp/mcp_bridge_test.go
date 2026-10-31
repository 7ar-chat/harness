package mcptools_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/mcp"
)

type noArgs struct{}

// connectMemory serves srv over an in-memory pipe and connects to it with the given result cap.
func connectMemory(t *testing.T, srv *mcp.Server, maxResultChars int) ([]ai.BaseTool, *mcp.ClientSession, error) {
	t.Helper()
	ct, st := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(context.Background(), st) }()
	ts, sess, err := mcptools.Connect(context.Background(), mcptools.Server{Name: "mem", ClientTransport: ct}, "test", 5*time.Second, maxResultChars)
	if sess != nil {
		t.Cleanup(func() { _ = sess.Close() })
	}
	return ts, sess, err
}

func serverWithTools(opts *mcp.ServerOptions, n int) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "many", Version: "1"}, opts)
	for i := range n {
		mcp.AddTool(srv, &mcp.Tool{Name: fmt.Sprintf("tool%02d", i), Description: "one of many"},
			func(context.Context, *mcp.CallToolRequest, noArgs) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
			})
	}
	return srv
}

// listMiddleware answers tools/list itself: next(page) gives the tools and the cursor of every page, or an error.
func listMiddleware(answer func(page int) (*mcp.ListToolsResult, error)) mcp.Middleware {
	page := 0
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/list" {
				return next(ctx, method, req)
			}
			page++
			res, err := answer(page)
			if err != nil {
				return nil, err
			}
			return res, nil
		}
	}
}

func TestConnectCollectsEveryPageOfAPaginatedToolList(t *testing.T) {
	// Arrange: 5 tools served 2 per page
	srv := serverWithTools(&mcp.ServerOptions{PageSize: 2}, 5)

	// Act
	ts, _, err := connectMemory(t, srv, 10000)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got := names(t, ts); len(got) != 5 {
		t.Fatalf("tools %v: a server's later pages must not be dropped", got)
	}
}

func TestConnectRefusesAServerWhoseToolListNeverEnds(t *testing.T) {
	// Arrange: every page points at yet another page
	srv := serverWithTools(nil, 1)
	srv.AddReceivingMiddleware(listMiddleware(func(page int) (*mcp.ListToolsResult, error) {
		return &mcp.ListToolsResult{Tools: []*mcp.Tool{{Name: fmt.Sprintf("t%d", page)}}, NextCursor: fmt.Sprintf("page-%d", page)}, nil
	}))

	// Act
	ts, sess, err := connectMemory(t, srv, 10000)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "exceeded max tool pages") || ts != nil || sess != nil {
		t.Fatalf("tools %v session %v err %v", ts, sess, err)
	}
}

func TestConnectRefusesAServerThatRepeatsACursor(t *testing.T) {
	// Arrange
	srv := serverWithTools(nil, 1)
	srv.AddReceivingMiddleware(listMiddleware(func(page int) (*mcp.ListToolsResult, error) {
		return &mcp.ListToolsResult{Tools: []*mcp.Tool{{Name: fmt.Sprintf("t%d", page)}}, NextCursor: "same"}, nil
	}))

	// Act
	_, _, err := connectMemory(t, srv, 10000)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "repeated cursor: same") {
		t.Fatalf("err %v", err)
	}
}

func TestConnectReportsAToolListThatFails(t *testing.T) {
	// Arrange
	srv := serverWithTools(nil, 1)
	srv.AddReceivingMiddleware(listMiddleware(func(int) (*mcp.ListToolsResult, error) { return nil, errors.New("catalog unavailable") }))

	// Act
	_, sess, err := connectMemory(t, srv, 10000)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "list mcp tools") || !strings.Contains(err.Error(), "catalog unavailable") || sess != nil {
		t.Fatalf("session %v err %v", sess, err)
	}
}

// resultServer has tools that answer with text, a tool error and a protocol error.
func resultServer(text string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "res", Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "dump", Description: "returns a lot"},
		func(context.Context, *mcp.CallToolRequest, noArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "fail", Description: "fails"},
		func(context.Context, *mcp.CallToolRequest, noArgs) (*mcp.CallToolResult, any, error) {
			return nil, nil, errors.New("quota exceeded")
		})
	return srv
}

func callTool(t *testing.T, ts []ai.BaseTool, name string) (string, error) {
	t.Helper()
	for _, x := range ts {
		if info, _ := x.Info(context.Background()); info.Name == name {
			return x.(ai.InvokableTool).InvokableRun(context.Background(), `{}`)
		}
	}
	t.Fatalf("tool %s not found in %v", name, names(t, ts))
	return "", nil
}

func TestAnOversizedToolResultIsCutToTheCapKeepingItsTail(t *testing.T) {
	// Arrange: a 5000-character answer whose last words matter, cap of 400 characters
	text := strings.Repeat("a", 5000) + " THE-END-MARKER"
	ts, _, err := connectMemory(t, resultServer(text), 400)
	if err != nil {
		t.Fatal(err)
	}

	// Act
	out, err := callTool(t, ts, "mem__dump")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if n := utf8.RuneCountInString(out); n > 400 {
		t.Fatalf("result has %d characters, cap is 400", n)
	}
	var doc struct {
		Content []struct{ Text string } `json:"content"`
		Meta    struct {
			Truncated     bool `json:"truncated"`
			OriginalChars int  `json:"originalChars"`
			ReturnedChars int  `json:"returnedChars"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("a truncated result must stay valid JSON: %v\n%s", err, out)
	}
	if !doc.Meta.Truncated || doc.Meta.OriginalChars < 5000 || doc.Meta.ReturnedChars <= 0 || len(doc.Content) != 1 {
		t.Fatalf("envelope %+v", doc)
	}
	if !strings.Contains(doc.Content[0].Text, "[truncated original_chars=") || !strings.Contains(doc.Content[0].Text, "THE-END-MARKER") {
		t.Fatalf("the cut text must carry the truncation marker and keep the tail: %q", doc.Content[0].Text)
	}
}

func TestAToolResultThatFitsTheCapIsReturnedWhole(t *testing.T) {
	// Arrange
	ts, _, err := connectMemory(t, resultServer("short answer"), 400)
	if err != nil {
		t.Fatal(err)
	}

	// Act
	out, err := callTool(t, ts, "mem__dump")

	// Assert
	if err != nil || !strings.Contains(out, "short answer") || strings.Contains(out, "truncated") {
		t.Fatalf("out %q err %v", out, err)
	}
}

func TestACapTooSmallForAnyResultIsAnError(t *testing.T) {
	// Arrange: even the empty envelope does not fit in 20 characters
	ts, _, err := connectMemory(t, resultServer(strings.Repeat("a", 500)), 20)
	if err != nil {
		t.Fatal(err)
	}

	// Act
	out, err := callTool(t, ts, "mem__dump")

	// Assert
	if err == nil || !strings.Contains(err.Error(), "exceeds max chars") || out != "" {
		t.Fatalf("out %q err %v", out, err)
	}
}

func TestAToolTheServerReportsAsFailedIsAnErrorCarryingItsMessage(t *testing.T) {
	// Arrange
	ts, _, err := connectMemory(t, resultServer("x"), 10000)
	if err != nil {
		t.Fatal(err)
	}

	// Act
	out, err := callTool(t, ts, "mem__fail")

	// Assert
	if out != "" || err == nil || !strings.Contains(err.Error(), "mcp server return error") || !strings.Contains(err.Error(), "quota exceeded") || !strings.Contains(err.Error(), `"isError":true`) {
		t.Fatalf("out %q err %v", out, err)
	}
}

func TestAToolCalledAfterItsSessionClosedIsAnError(t *testing.T) {
	// Arrange
	ts, sess, err := connectMemory(t, resultServer("x"), 10000)
	if err != nil {
		t.Fatal(err)
	}
	_ = sess.Close()

	// Act
	_, err = callTool(t, ts, "mem__dump")

	// Assert
	if err == nil || !strings.Contains(err.Error(), "failed to call mcp tool") {
		t.Fatalf("err %v", err)
	}
}

func TestProviderSkipsAServerThatCannotBeReachedAndKeepsTheOthers(t *testing.T) {
	// Arrange: the first server refuses connections
	ts, seen := kbServer(t)
	_ = seen
	p := mcptools.New([]config.MCPServer{
		{Name: "down", URL: "http://127.0.0.1:1/mcp"},
		{Name: "kb", URL: ts.URL, Tools: []string{"search"}},
	}, "test", 10000)
	p.ConnectTimeout = 2 * time.Second

	// Act
	list, cleanup, err := p.Tools(context.Background(), tools.RunContext{RunID: "r"})
	defer cleanup()

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got := names(t, list); len(got) != 1 || got[0] != "kb__search" || p.Name() != "mcp" {
		t.Fatalf("tools %v provider %q", got, p.Name())
	}
}

func TestProviderWithoutServersIsNil(t *testing.T) {
	if p := mcptools.New(nil, "test", 10); p != nil {
		t.Fatalf("provider %+v", p)
	}
	if p := mcptools.NewWithPending(nil, nil, "test", 10); p != nil {
		t.Fatalf("provider %+v", p)
	}
}

func TestApprovalToolNamesOfLongServerNamesStayWithinTheModelLimitAndAreDistinct(t *testing.T) {
	// Arrange: two pending servers with 80-character names that differ only at the end
	long := strings.Repeat("s", 80)
	a := projectStdio()
	a.Name = long + "a"
	b := projectStdio()
	b.Name = long + "b"
	p := mcptools.NewWithPending(nil, []config.MCPServer{a, b}, "test", 10)
	p.TrustDir, p.Ask = t.TempDir(), true

	// Act
	list, cleanup, err := p.Tools(context.Background(), tools.RunContext{RunID: "r"})
	defer cleanup()

	// Assert
	if err != nil || len(list) != 2 {
		t.Fatalf("tools %d err %v", len(list), err)
	}
	seen := map[string]bool{}
	for _, n := range names(t, list) {
		if len(n) > 64 {
			t.Errorf("tool name %q is %d characters, the limit is 64", n, len(n))
		}
		seen[n] = true
	}
	if len(seen) != 2 {
		t.Fatalf("names collide: %v", seen)
	}
}

func TestThePendingServerIsNotOfferedWhenTheThreadHistoryCannotBeRead(t *testing.T) {
	// Arrange: fail closed, a server whose question may already have been asked is not asked again
	p, _, _ := pendingProvider(t, projectStdio(), store.InteractionInteractive)
	p.History = func(context.Context, string) ([]*store.Message, error) { return nil, errors.New("db down") }

	// Act
	list, cleanup, err := p.Tools(context.Background(), tools.RunContext{RunID: "r", ThreadID: "t"})
	defer cleanup()

	// Assert
	if err != nil || len(list) != 0 {
		t.Fatalf("tools %v err %v", names(t, list), err)
	}
}

func TestApprovingAServerWhoseTrustFileCannotBeWrittenTellsTheModel(t *testing.T) {
	// Arrange: the trust directory is read-only, so the approval cannot be recorded
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	p, _, _ := pendingProvider(t, projectStdio(), store.InteractionInteractive)
	if err := os.Chmod(p.TrustDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p.TrustDir, 0o700) })
	ctx := context.Background()
	list, cleanup, _ := p.Tools(ctx, tools.RunContext{RunID: "r", ThreadID: "t"})
	defer cleanup()
	_, approve := approvalTool(t, list)

	// Act
	out, err := approve.InvokableRun(ctx, "{}")

	// Assert
	if err == nil || !strings.Contains(err.Error(), "the approval could not be recorded") || out != "" {
		t.Fatalf("out %q err %v", out, err)
	}
	if ok, _ := config.MCPApproved(p.TrustDir, "/repo", projectStdio()); ok {
		t.Fatal("a failed approval must not count as approved")
	}
}
