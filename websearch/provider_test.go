package websearch_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GhaziBenDahmane/harness/agent"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/tools"
	"github.com/GhaziBenDahmane/harness/websearch"
)

type queryIn struct {
	Query string `json:"query"`
}

type linkIn struct {
	Link string `json:"link"`
}

// mcpServer is a fake web search MCP: tavily_search, scrape_webpage and crawl
// (not allowed). It records the Authorization of every request; reject makes it
// answer 401 to tool calls.
type mcpServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	auth   []string
	reject atomic.Bool
}

func newMCPServer(t *testing.T) *mcpServer {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "websearch", Version: "1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "tavily_search", Description: "Search the web"},
		func(_ context.Context, _ *mcp.CallToolRequest, in queryIn) (*mcp.CallToolResult, any, error) {
			body := `{"query":"` + in.Query + `","results":[{"title":"Go","url":"https://go.dev","content":"` + strings.Repeat("x", 5000) + `"}]}`
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: body}}}, nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "scrape_webpage", Description: "Scrape a page"},
		func(_ context.Context, _ *mcp.CallToolRequest, in linkIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "# Page " + in.Link}}}, nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "crawl", Description: "Not allowed"},
		func(context.Context, *mcp.CallToolRequest, queryIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})
	m := &mcpServer{}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.auth = append(m.auth, r.Header.Get("Authorization"))
		m.mu.Unlock()
		if m.reject.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mcpServer) seen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.auth...)
}

func toolNames(t *testing.T, ts []tool.BaseTool) []string {
	t.Helper()
	var out []string
	for _, x := range ts {
		info, _ := x.Info(context.Background())
		out = append(out, info.Name)
	}
	sort.Strings(out)
	return out
}

func invoke(t *testing.T, ts []tool.BaseTool, name, args string) (string, error) {
	t.Helper()
	for _, x := range ts {
		if info, _ := x.Info(context.Background()); info.Name == name {
			return x.(tool.InvokableTool).InvokableRun(context.Background(), args)
		}
	}
	t.Fatalf("tool %s not found", name)
	return "", nil
}

func runCtx(r *store.Run) tools.RunContext {
	return tools.RunContext{ThreadID: r.ThreadID, RunID: r.ID, Login: r.UserLogin}
}

func TestProviderExposesOnlyTheAllowedToolsWithTheRunBearer(t *testing.T) {
	// Only tavily_search and scrape_webpage are exposed (websearch__*), every request carries the run user's bearer, results respect the output cap.
	// Arrange
	srv := newMCPServer(t)
	v, st := newVault(t)
	run := activeRun(t, st, "alice")
	_, _ = v.Put(context.Background(), run.ID, "alice", websearch.Token{Value: "alice-token", ExpiresAt: time.Now().Add(time.Hour)}, true)
	p := &websearch.Provider{URL: srv.srv.URL, Vault: v, Version: "test", MaxResultChars: 4000}

	// Act
	list, cleanup, err := p.Tools(context.Background(), runCtx(run))
	if err != nil {
		t.Fatal(err)
	}
	section := p.PromptSection(context.Background(), agent.Input{Run: run})
	out, callErr := invoke(t, list, websearch.SearchToolName, `{"query":"golang"}`)
	page, _ := invoke(t, list, websearch.ScrapeToolName, `{"link":"https://go.dev"}`)
	cleanup()
	after := p.PromptSection(context.Background(), agent.Input{Run: run})

	// Assert
	if got := toolNames(t, list); strings.Join(got, ",") != "websearch__scrape_webpage,websearch__tavily_search" {
		t.Fatalf("tools %v", got)
	}
	if callErr != nil || !strings.Contains(out, "golang") || len([]rune(out)) > 4000 || !strings.Contains(out, "truncated") || !strings.Contains(page, "Page https://go.dev") {
		t.Fatalf("out %d %q err %v page %q", len(out), out[:min(len(out), 80)], callErr, page)
	}
	for _, a := range srv.seen() {
		if a != "Bearer alice-token" {
			t.Fatalf("authorization %q", a)
		}
	}
	if !strings.Contains(section, websearch.SearchToolName) || after != "" {
		t.Fatalf("section %q after %q", section, after)
	}
}

func TestProviderNeverSharesATokenAcrossUsers(t *testing.T) {
	// Two concurrent runs of different users each call the MCP with their own token.
	// Arrange
	srv := newMCPServer(t)
	v, st := newVault(t)
	ctx := context.Background()
	ra := activeRun(t, st, "alice")
	rb := activeRun(t, st, "bob")
	_, _ = v.Put(ctx, ra.ID, "alice", websearch.Token{Value: "alice-token", ExpiresAt: time.Now().Add(time.Hour)}, true)
	_, _ = v.Put(ctx, rb.ID, "bob", websearch.Token{Value: "bob-token", ExpiresAt: time.Now().Add(time.Hour)}, true)
	p := &websearch.Provider{URL: srv.srv.URL, Vault: v, Version: "test", MaxResultChars: 30000}
	la, ca, _ := p.Tools(ctx, runCtx(ra))
	defer ca()
	before := len(srv.seen())
	lb, cb, _ := p.Tools(ctx, runCtx(rb))
	defer cb()
	bobConnect := srv.seen()[before:]

	// Act
	mark := len(srv.seen())
	_, errA := invoke(t, la, websearch.SearchToolName, `{"query":"a"}`)
	aliceCall := srv.seen()[mark:]
	mark = len(srv.seen())
	_, errB := invoke(t, lb, websearch.SearchToolName, `{"query":"b"}`)
	bobCall := srv.seen()[mark:]
	// A run of alice's id requested as bob gets nothing.
	lx, _, _ := p.Tools(ctx, tools.RunContext{RunID: ra.ID, Login: "bob"})

	// Assert
	if errA != nil || errB != nil || len(lx) != 0 {
		t.Fatalf("errs %v %v cross %d", errA, errB, len(lx))
	}
	check := func(name string, got []string, want string) {
		if len(got) == 0 {
			t.Fatalf("%s: no request", name)
		}
		for _, a := range got {
			if a != want {
				t.Fatalf("%s: authorization %q, want %q", name, a, want)
			}
		}
	}
	check("bob connect", bobConnect, "Bearer bob-token")
	check("alice call", aliceCall, "Bearer alice-token")
	check("bob call", bobCall, "Bearer bob-token")
}

func TestProviderIsNotRequired(t *testing.T) {
	// No token, an expired token or an unreachable server: no tools, no error, the run goes on.
	// Arrange
	srv := newMCPServer(t)
	v, st := newVault(t)
	ctx := context.Background()
	noToken := activeRun(t, st, "alice")
	expired := activeRun(t, st, "alice")
	down := activeRun(t, st, "alice")
	_, _ = v.Put(ctx, expired.ID, "alice", websearch.Token{Value: "old", ExpiresAt: time.Now().Add(time.Minute)}, true)
	_, _ = v.Put(ctx, down.ID, "alice", websearch.Token{Value: "tok", ExpiresAt: time.Now().Add(time.Hour)}, true)
	p := &websearch.Provider{URL: srv.srv.URL, Vault: &websearch.Vault{Store: st, Sealer: v.Sealer, Now: func() time.Time { return time.Now().Add(time.Hour) }}}
	dead := &websearch.Provider{URL: "http://127.0.0.1:1/mcp", Vault: v, ConnectTimeout: 2 * time.Second}

	// Act
	l1, _, e1 := p.Tools(ctx, runCtx(noToken))
	l2, _, e2 := p.Tools(ctx, runCtx(expired))
	l3, _, e3 := dead.Tools(ctx, runCtx(down))

	// Assert
	if len(l1)+len(l2)+len(l3) != 0 || e1 != nil || e2 != nil || e3 != nil {
		t.Fatalf("tools %d %d %d errs %v %v %v", len(l1), len(l2), len(l3), e1, e2, e3)
	}
	if len(srv.seen()) != 0 {
		t.Fatalf("connected without a valid token: %v", srv.seen())
	}
	if dead.Available(down.ID) || dead.PromptSection(ctx, agent.Input{Run: down}) != "" {
		t.Fatal("unavailable web search is announced")
	}
}

func TestProviderToolErrorsAreClearWhenTheTokenExpiresOrIsRefused(t *testing.T) {
	// Mid-run: an expired token gives MsgExpired without calling the server; a 401 gives MsgRejected; a steer refresh is picked up.
	// Arrange
	srv := newMCPServer(t)
	v, st := newVault(t)
	ctx := context.Background()
	run := activeRun(t, st, "alice")
	base := time.Now()
	_, _ = v.Put(ctx, run.ID, "alice", websearch.Token{Value: "first", ExpiresAt: base.Add(10 * time.Minute)}, true)
	p := &websearch.Provider{URL: srv.srv.URL, Vault: v, MaxResultChars: 30000}
	list, cleanup, _ := p.Tools(ctx, runCtx(run))
	defer cleanup()

	// Act
	_, _ = v.Put(ctx, run.ID, "alice", websearch.Token{Value: "steered", ExpiresAt: base.Add(time.Hour)}, false)
	mark := len(srv.seen())
	_, refreshedErr := invoke(t, list, websearch.SearchToolName, `{"query":"q"}`)
	refreshed := srv.seen()[mark:]
	srv.reject.Store(true)
	_, rejectedErr := invoke(t, list, websearch.SearchToolName, `{"query":"q"}`)
	srv.reject.Store(false)
	v.Now = func() time.Time { return base.Add(2 * time.Hour) }
	mark = len(srv.seen())
	_, expiredErr := invoke(t, list, websearch.ScrapeToolName, `{"link":"https://x"}`)

	// Assert
	if refreshedErr != nil || len(refreshed) == 0 || refreshed[0] != "Bearer steered" {
		t.Fatalf("refresh err %v auth %v", refreshedErr, refreshed)
	}
	var ue *tools.UserError
	if !errors.As(rejectedErr, &ue) || ue.Msg != websearch.MsgRejected {
		t.Fatalf("rejected %v", rejectedErr)
	}
	if !errors.As(expiredErr, &ue) || ue.Msg != websearch.MsgExpired || len(srv.seen()) != mark {
		t.Fatalf("expired %v requests %d→%d", expiredErr, mark, len(srv.seen()))
	}
}

func TestProviderNeverSendsTheTokenToARedirectTarget(t *testing.T) {
	// WEB_SEARCH_MCP_URL redirecting to another host: the bearer is not re-sent there (the connection fails, web search is not offered).
	// Arrange
	srv := newMCPServer(t)
	var foreign []string
	var mu sync.Mutex
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		foreign = append(foreign, r.Header.Get("Authorization"))
		mu.Unlock()
		http.Redirect(w, r, srv.srv.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(elsewhere.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)
	v, st := newVault(t)
	run := activeRun(t, st, "alice")
	_, _ = v.Put(context.Background(), run.ID, "alice", websearch.Token{Value: "alice-token", ExpiresAt: time.Now().Add(time.Hour)}, true)
	p := &websearch.Provider{URL: redirector.URL + "/mcp", Vault: v, Version: "test", MaxResultChars: 4000, ConnectTimeout: 2 * time.Second}

	// Act
	list, _, err := p.Tools(context.Background(), runCtx(run))

	// Assert
	mu.Lock()
	defer mu.Unlock()
	if err != nil || len(list) != 0 || len(srv.seen()) != 0 {
		t.Fatalf("err %v tools %d mcp requests %v", err, len(list), srv.seen())
	}
	for _, a := range foreign {
		if a != "" {
			t.Fatalf("token sent to the redirect target: %q", a)
		}
	}
}
