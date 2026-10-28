// Package mcptools exposes tools of configured MCP servers (WORK_MCP_SERVERS).
package mcptools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/cloudwego/eino-ext/components/tool/mcp/officialmcp"
	"github.com/cloudwego/eino/components/tool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GhaziBenDahmane/harness/auth"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/tools"
)

// Provider connects every configured MCP server per run (identity is per user).
type Provider struct {
	servers []config.MCPServer
	version string
	// ConnectTimeout bounds connect + list tools per server.
	ConnectTimeout time.Duration
	// MaxResultChars caps a tool result (officialmcp result policy).
	MaxResultChars int
	// CallTimeout bounds one tool call (0: DefaultCallTimeout).
	CallTimeout time.Duration
}

// DefaultCallTimeout bounds one MCP tool call (MCP_CALL_TIMEOUT_SECONDS).
const DefaultCallTimeout = 120 * time.Second

// MaxResultChars is the result cap that leaves large results to the agent's
// spill (harness v3 §2: the full output goes to a file, nothing is cut silently).
const MaxResultChars = 1 << 20

// New returns nil when no servers are configured.
func New(servers []config.MCPServer, version string, maxResultChars int) *Provider {
	if len(servers) == 0 {
		return nil
	}
	return &Provider{servers: servers, version: version, ConnectTimeout: 15 * time.Second, MaxResultChars: maxResultChars}
}

func (p *Provider) Name() string { return "mcp" }

type headerTransport struct {
	base     http.RoundTripper
	headers  map[string]string
	userinfo string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	if t.userinfo != "" {
		req.Header.Set(auth.HeaderName, t.userinfo)
	}
	return t.base.RoundTrip(req)
}

var invalidName = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// ToolName is the exposed name of an MCP tool: {server}__{tool}, sanitized, ≤ 64 chars.
func ToolName(server, name string) string {
	n := invalidName.ReplaceAllString(server+"__"+name, "_")
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

// uniqueName is ToolName, made unique among taken with a short hash of the
// original name when sanitizing or truncating made two tools collide (A-23).
func uniqueName(taken map[string]bool, server, name string) string {
	n := ToolName(server, name)
	sum := sha256.Sum256([]byte(server + "__" + name))
	h := hex.EncodeToString(sum[:])
	for k := 4; taken[n] && k <= len(h); k += 2 {
		base := ToolName(server, name)
		if len(base) > 64-k-1 {
			base = base[:64-k-1]
		}
		n = base + "_" + h[:k]
	}
	taken[n] = true
	return n
}

// Tools implements tools.Provider. Failing servers are logged and skipped.
func (p *Provider) Tools(ctx context.Context, rc tools.RunContext) ([]tool.BaseTool, func(), error) {
	var all []tool.BaseTool
	var sessions []*mcp.ClientSession
	taken := map[string]bool{} // exposed names are unique across the servers
	for _, s := range p.servers {
		ts, sess, err := p.connect(ctx, s, rc, taken)
		if err != nil {
			slog.Warn("mcp server unavailable, skipping", "server", s.Name, "run_id", rc.RunID, "error", err)
			continue
		}
		sessions = append(sessions, sess)
		all = append(all, ts...)
	}
	return all, func() {
		for _, s := range sessions {
			_ = s.Close()
		}
	}, nil
}

func (p *Provider) connect(ctx context.Context, s config.MCPServer, rc tools.RunContext, taken map[string]bool) ([]tool.BaseTool, *mcp.ClientSession, error) {
	tr := &headerTransport{base: http.DefaultTransport, headers: s.Headers}
	if s.ForwardIdentity {
		tr.userinfo = rc.Userinfo
	}
	return Connect(ctx, Server{Name: s.Name, URL: s.URL, Tools: s.Tools, Transport: tr, CallTimeout: p.CallTimeout, taken: taken},
		p.version, p.ConnectTimeout, p.MaxResultChars)
}

// Server is an MCP server connected for one run.
type Server struct {
	Name string
	URL  string
	// Tools is the allow-list of the server's tools (empty: all of them).
	Tools []string
	// Transport adds the server's auth headers (nil: http.DefaultTransport).
	Transport http.RoundTripper
	// CallTimeout bounds one tool call (0: DefaultCallTimeout).
	CallTimeout time.Duration
	// taken are the exposed names already used by the run (nil: this server's only).
	taken map[string]bool
}

// Connect opens a session to s and returns its allowed tools, exposed as
// ToolName(server, tool) with results capped at maxResultChars. connectTimeout
// bounds connect + list tools; the session (closed by the caller) outlives it.
func Connect(ctx context.Context, s Server, version string, connectTimeout time.Duration, maxResultChars int) ([]tool.BaseTool, *mcp.ClientSession, error) {
	tr := s.Transport
	if tr == nil {
		tr = http.DefaultTransport
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "studio-work", Version: version}, nil)
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	// The session outlives the connect timeout: it is bound to the transport, not cctx.
	sess, err := client.Connect(cctx, &mcp.StreamableClientTransport{
		Endpoint:             s.URL,
		HTTPClient:           &http.Client{Transport: tr},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return nil, nil, err
	}
	errAsError := true
	// Original "{server}__{tool}" names per exposed name: the approval gate
	// decides riskiness from them, not from the sanitized/truncated name.
	originals := map[string][]string{}
	readOnly := map[string]bool{}
	taken := s.taken
	if taken == nil {
		taken = map[string]bool{}
	}
	exposedOf := map[string]string{} // original tool name → exposed (the mapper may run more than once)
	ts, err := officialmcp.GetTools(cctx, &officialmcp.Config{
		Cli:           sess,
		ServerName:    s.Name,
		ToolNameList:  s.Tools,
		ListToolsMode: officialmcp.ListToolsAllPages,
		ToolNameMapper: func(_ context.Context, in officialmcp.ToolNameMapperInput) (officialmcp.ToolNameMapperOutput, error) {
			exposed, ok := exposedOf[in.Tool.Name]
			if !ok {
				exposed = uniqueName(taken, in.ServerName, in.Tool.Name)
				exposedOf[in.Tool.Name] = exposed
				originals[exposed] = []string{in.ServerName + "__" + in.Tool.Name}
				readOnly[exposed] = in.Tool.Annotations != nil && in.Tool.Annotations.ReadOnlyHint
			}
			return officialmcp.ToolNameMapperOutput{ExposedName: exposed}, nil
		},
		ResultPolicy: &officialmcp.ResultPolicy{
			MaxChars:          maxResultChars,
			PreserveTailChars: 2000,
			ErrorAsError:      &errAsError,
		},
	})
	if err != nil {
		_ = sess.Close()
		return nil, nil, err
	}
	for i, t := range ts {
		inv, ok := t.(tool.InvokableTool)
		if !ok {
			continue
		}
		info, err := t.Info(cctx)
		if err != nil || len(originals[info.Name]) == 0 {
			continue
		}
		timeout := s.CallTimeout
		if timeout <= 0 {
			timeout = DefaultCallTimeout
		}
		nt := &namedTool{InvokableTool: inv, names: append([]string{info.Name}, originals[info.Name]...), timeout: timeout}
		if readOnly[info.Name] {
			ts[i] = &readOnlyTool{nt}
		} else {
			ts[i] = nt
		}
	}
	return ts, sess, nil
}

// namedTool is an MCP tool that also reports its original names
// (tools.ApprovalNamer) and bounds every call by its timeout.
type namedTool struct {
	tool.InvokableTool
	names   []string
	timeout time.Duration
}

func (t *namedTool) ApprovalNames() []string { return t.names }

// errCallTimeout is the model-facing error of an MCP call that took too long.
var errCallTimeout = errors.New("the MCP tool call timed out")

// InvokableRun calls the tool within its timeout.
func (t *namedTool) InvokableRun(ctx context.Context, args string, opts ...tool.Option) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	out, err := t.InvokableTool.InvokableRun(cctx, args, opts...)
	if err != nil && ctx.Err() == nil && errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return "", tools.Errorf("%v after %s; try again later or with a smaller request", errCallTimeout, t.timeout)
	}
	return out, err
}

// readOnlyTool is an MCP tool annotated readOnlyHint (tools.ReadOnly: may run in parallel).
type readOnlyTool struct{ *namedTool }

func (readOnlyTool) ReadOnly() bool { return true }
