// Package mcptools exposes tools of configured MCP servers (MCP_SERVERS).
package mcptools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/tools"
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
	// TrustDir is the user directory holding trust.json. A stdio server a project defines
	// (config.MCPServer.ProjectRoot) is only spawned when the user approved its exact command there
	// (config.MCPApproved); config.Load already withholds the others, this is the check at the spawn.
	TrustDir string
	// Pending are the project stdio servers that wait for the user's approval (config.Layers.MCPPending).
	// Ask offers the model a tool that asks for it (approve.go); set it only when the run's approval gate
	// pauses such a call and refuses it where nobody can answer (approvals.Gate). History reads a thread's
	// messages so a refused question is not asked again on the thread.
	Pending []config.MCPServer
	Ask     bool
	History func(ctx context.Context, threadID string) ([]*store.Message, error)
}

// ErrNotApproved means a project's stdio server was not spawned: its command has no approval (or a changed one).
var ErrNotApproved = errors.New("the project's stdio MCP server is not approved: approve it when asked, or run `harness trust mcp <name>`")

// DefaultCallTimeout bounds one MCP tool call (MCP_CALL_TIMEOUT_SECONDS).
const DefaultCallTimeout = 120 * time.Second

// MaxResultChars is the result cap that leaves large results to the agent's
// spill (harness v3 §2: the full output goes to a file, nothing is cut silently).
const MaxResultChars = 1 << 20

// New returns nil when no servers are configured.
func New(servers []config.MCPServer, version string, maxResultChars int) *Provider {
	return NewWithPending(servers, nil, version, maxResultChars)
}

// NewWithPending is New for a configuration that also has project stdio servers waiting for approval; it returns
// nil when there is neither.
func NewWithPending(servers, pending []config.MCPServer, version string, maxResultChars int) *Provider {
	if len(servers) == 0 && len(pending) == 0 {
		return nil
	}
	return &Provider{servers: servers, Pending: pending, version: version, ConnectTimeout: 15 * time.Second, MaxResultChars: maxResultChars}
}

func (p *Provider) Name() string { return "mcp" }

type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	return t.base.RoundTrip(req)
}

// ToolName is the exposed name of an MCP tool: {server}__{tool}, sanitized, ≤ 64 chars.
func ToolName(server, name string) string { return config.MCPToolName(server, name) }

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
func (p *Provider) Tools(ctx context.Context, rc tools.RunContext) ([]ai.BaseTool, func(), error) {
	var all []ai.BaseTool
	var sessions []*mcp.ClientSession
	taken := map[string]bool{} // exposed names are unique across the servers
	offer, approved := p.pendingTools(ctx, rc)
	all = append(all, offer...)
	for _, s := range append(slices.Clone(p.servers), approved...) {
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

func (p *Provider) connect(ctx context.Context, s config.MCPServer, rc tools.RunContext, taken map[string]bool) ([]ai.BaseTool, *mcp.ClientSession, error) {
	tr := &headerTransport{base: http.DefaultTransport, headers: s.Headers}
	if s.IsStdio() && s.ProjectRoot != "" {
		ok, err := config.MCPApproved(p.TrustDir, s.ProjectRoot, s)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, ErrNotApproved
		}
	}
	srv := Server{Name: s.Name, URL: s.URL, Tools: s.Tools, Transport: tr, CallTimeout: p.CallTimeout, SSE: s.Type == "sse", taken: taken}
	if s.IsStdio() {
		srv.ClientTransport = &mcp.CommandTransport{Command: StdioCommand(s)}
	}
	return Connect(ctx, srv,
		p.version, p.ConnectTimeout, p.MaxResultChars)
}

// stdioEnvPassthrough are the variables a stdio server inherits; the others
// (API keys, database passwords) stay with the harness unless the server's env lists them.
var stdioEnvPassthrough = []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TERM", "TMPDIR", "SHELL"}

// StdioCommand prepares the child process of a stdio server with a reduced environment.
func StdioCommand(s config.MCPServer) *exec.Cmd {
	cmd := exec.Command(s.Command, s.Args...)
	for _, k := range stdioEnvPassthrough {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	for k, v := range s.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return cmd
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
	// SSE connects with the legacy HTTP+SSE transport instead of streamable HTTP.
	SSE bool
	// ClientTransport, when set, replaces the HTTP transports (a child process, an in-memory pipe in tests).
	ClientTransport mcp.Transport
	// taken are the exposed names already used by the run (nil: this server's only).
	taken map[string]bool
}

// Connect opens a session to s and returns its allowed tools, exposed as
// ToolName(server, tool) with results capped at maxResultChars. connectTimeout
// bounds connect + list tools; the session (closed by the caller) outlives it.
func Connect(ctx context.Context, s Server, version string, connectTimeout time.Duration, maxResultChars int) ([]ai.BaseTool, *mcp.ClientSession, error) {
	tr := s.Transport
	if tr == nil {
		tr = http.DefaultTransport
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "harness", Version: version}, nil)
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	// The session outlives the connect timeout: it is bound to the transport, not cctx.
	var transport mcp.Transport
	switch {
	case s.ClientTransport != nil:
		transport = s.ClientTransport
	case s.SSE:
		transport = &mcp.SSEClientTransport{Endpoint: s.URL, HTTPClient: &http.Client{Transport: tr}}
	default:
		transport = &mcp.StreamableClientTransport{Endpoint: s.URL, HTTPClient: &http.Client{Transport: tr}, DisableStandaloneSSE: true}
	}
	connectCtx := cctx
	if s.SSE || s.ClientTransport != nil {
		// The SSE stream and a child process live as long as the context given to Connect:
		// it must outlive the connect timeout, which only bounds the handshake.
		var stop context.CancelFunc
		connectCtx, stop = context.WithCancel(context.WithoutCancel(ctx))
		timer := time.AfterFunc(connectTimeout, stop)
		defer timer.Stop()
	}
	sess, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		return nil, nil, err
	}
	// Original "{server}__{tool}" names per exposed name: the approval gate
	// decides riskiness from them, not from the sanitized/truncated name.
	originals := map[string][]string{}
	readOnly := map[string]bool{}
	taken := s.taken
	if taken == nil {
		taken = map[string]bool{}
	}
	exposedOf := map[string]string{} // original tool name → exposed
	ts, err := listBridgeTools(cctx, bridgeConfig{
		Session: sess, ServerName: s.Name, Only: s.Tools, MaxChars: maxResultChars, PreserveTail: 2000,
		Expose: func(server string, t *mcp.Tool) string {
			exposed, ok := exposedOf[t.Name]
			if !ok {
				exposed = uniqueName(taken, server, t.Name)
				exposedOf[t.Name] = exposed
				originals[exposed] = []string{server + "__" + t.Name}
				readOnly[exposed] = t.Annotations != nil && t.Annotations.ReadOnlyHint
			}
			return exposed
		},
	})
	if err != nil {
		_ = sess.Close()
		return nil, nil, err
	}
	for i, t := range ts {
		inv, ok := t.(ai.InvokableTool)
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
	ai.InvokableTool
	names   []string
	timeout time.Duration
}

func (t *namedTool) ApprovalNames() []string { return t.names }

// errCallTimeout is the model-facing error of an MCP call that took too long.
var errCallTimeout = errors.New("the MCP tool call timed out")

// InvokableRun calls the tool within its timeout.
func (t *namedTool) InvokableRun(ctx context.Context, args string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	out, err := t.InvokableTool.InvokableRun(cctx, args)
	if err != nil && ctx.Err() == nil && errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return "", tools.Errorf("%v after %s; try again later or with a smaller request", errCallTimeout, t.timeout)
	}
	return out, err
}

// readOnlyTool is an MCP tool annotated readOnlyHint (tools.ReadOnly: may run in parallel).
type readOnlyTool struct{ *namedTool }

func (readOnlyTool) ReadOnly() bool { return true }
