package mcptools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/7ar-chat/harness/config"
)

// ToolInfo describes one tool a server offers.
type ToolInfo struct {
	Name        string
	Description string
	// ReadOnly is the server's own readOnlyHint annotation.
	ReadOnly bool
}

// ErrRejected is wrapped by the error of ListTools when the server answered 401 or 403: the credentials were not
// accepted.
var ErrRejected = errors.New("the server rejected the credentials")

// statusTransport remembers whether a response was an authentication failure.
type statusTransport struct {
	base     http.RoundTripper
	rejected atomic.Bool
}

func (t *statusTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		t.rejected.Store(true)
	}
	return resp, err
}

// ListTools connects to a database-supplied server (guarded transport, no stdio; s.Type "sse" or streamable HTTP,
// s.Headers sent) and lists ALL its tools, ignoring s.Tools. The session is closed before it returns. An
// authentication failure wraps ErrRejected.
func ListTools(ctx context.Context, s config.MCPServer, version string, timeout time.Duration) ([]ToolInfo, error) {
	return listTools(ctx, s, version, timeout, SafeTransport())
}

func listTools(ctx context.Context, s config.MCPServer, version string, timeout time.Duration, base http.RoundTripper) ([]ToolInfo, error) {
	if s.IsStdio() {
		return nil, errUntrustedStdio
	}
	if s.URL == "" {
		return nil, errors.New("the server has no URL")
	}
	st := &statusTransport{base: &headerTransport{base: base, headers: s.Headers}}
	srv := Server{Name: s.Name, URL: s.URL, Transport: st, SSE: s.Type == "sse", Untrusted: true}
	sess, cctx, release, err := dial(ctx, srv, version, timeout)
	if err == nil {
		defer release()
		defer sess.Close()
		var raw []*mcp.Tool
		if raw, err = listAllTools(cctx, sess); err == nil {
			out := make([]ToolInfo, 0, len(raw))
			for _, t := range raw {
				out = append(out, ToolInfo{Name: t.Name, Description: t.Description, ReadOnly: t.Annotations != nil && t.Annotations.ReadOnlyHint})
			}
			return out, nil
		}
	}
	if st.rejected.Load() {
		return nil, fmt.Errorf("%w", ErrRejected)
	}
	return nil, err
}
