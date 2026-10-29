// Package example is a worked sandbox adapter for a generic REST service that
// can create sandboxes and run commands in them. Copy it, change the wire
// format to your provider's, and run sandboxtest.RunConformance on it (see
// rest_test.go and docs/sandbox-adapters.md).
//
// The service this adapter speaks to (JSON unless noted):
//
//	POST   /sandboxes                {"metadata":{}}             -> {"id":"…"}
//	GET    /sandboxes/{id}                                       -> 200 | 404 (gone)
//	DELETE /sandboxes/{id}
//	POST   /sandboxes/{id}/renew     {"seconds":1800}
//	POST   /sandboxes/{id}/exec      {"command","cwd","timeout_ms","env"}
//	                                 -> NDJSON stream: {"o":"…"} {"e":"…"} … {"exit":N}
//	GET    /sandboxes/{id}/file?path=&max=   -> raw bytes, header X-File-Size; 404
//	PUT    /sandboxes/{id}/file?path=        raw body (creates parents)
//	POST   /sandboxes/{id}/mkdir?path=
//	GET    /sandboxes/{id}/list?path=&depth= -> [{"path","type","size","mtime"}]; 404
//	GET    /sandboxes/{id}/stat?path=        -> {"path","type","size","mtime"}; 404
package example

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/GhaziBenDahmane/harness/core/sandbox"
)

// Backend is the sandbox.Backend of the REST service at BaseURL.
type Backend struct {
	BaseURL string
	Token   string       // sent as a bearer token when set
	HTTP    *http.Client // default http.DefaultClient (no overall timeout: Exec streams)
}

var _ sandbox.Backend = (*Backend)(nil)

func (b *Backend) client() *http.Client {
	if b.HTTP != nil {
		return b.HTTP
	}
	return http.DefaultClient
}

// do sends a request and maps the provider's errors to the sandbox package's.
func (b *Backend) do(ctx context.Context, method, path string, q url.Values, body io.Reader, in any) (*http.Response, error) {
	if in != nil {
		buf, _ := json.Marshal(in)
		body = bytes.NewReader(buf)
	}
	u := b.BaseURL + path
	if q != nil {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if b.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.Token)
	}
	resp, err := b.client().Do(req)
	if err != nil {
		return nil, err // network errors are transient: never ErrNoSandbox
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		switch {
		case resp.StatusCode == http.StatusNotFound && resp.Header.Get("X-Not-Found") == "sandbox":
			return nil, fmt.Errorf("%w: %s", sandbox.ErrGone, msg)
		case resp.StatusCode == http.StatusNotFound:
			return nil, sandbox.ErrNotFound // a missing file or directory
		case resp.StatusCode == http.StatusTooManyRequests:
			return nil, fmt.Errorf("%w: %s", sandbox.ErrQuota, msg)
		}
		return nil, fmt.Errorf("%w: %s %s: %s", sandbox.ErrUnavailable, method, path, resp.Status)
	}
	return resp, nil
}

// Create makes a sandbox.
func (b *Backend) Create(ctx context.Context, o sandbox.CreateOptions) (sandbox.Instance, error) {
	resp, err := b.do(ctx, "POST", "/sandboxes", nil, nil, map[string]any{"metadata": o.Metadata})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct{ ID string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.ID == "" {
		return nil, fmt.Errorf("create sandbox: bad response (%v)", err)
	}
	return &Instance{b: b, id: out.ID}, nil
}

// Connect reattaches to a sandbox. ErrNoSandbox only when it definitively no
// longer exists; every other failure is transient (the manager keeps the sandbox).
func (b *Backend) Connect(ctx context.Context, id string) (sandbox.Instance, error) {
	resp, err := b.do(ctx, "GET", "/sandboxes/"+url.PathEscape(id), nil, nil, nil)
	if err != nil {
		if sandbox.IsGone(err) {
			return nil, sandbox.ErrNoSandbox
		}
		return nil, err
	}
	resp.Body.Close()
	return &Instance{b: b, id: id}, nil
}

// Kill destroys a sandbox; an already missing one is not an error.
func (b *Backend) Kill(ctx context.Context, id string) error {
	resp, err := b.do(ctx, "DELETE", "/sandboxes/"+url.PathEscape(id), nil, nil, nil)
	if sandbox.IsGone(err) {
		return nil
	}
	if err == nil {
		resp.Body.Close()
	}
	return err
}

// Instance is one sandbox of the service.
type Instance struct {
	b  *Backend
	id string
}

var _ sandbox.Instance = (*Instance)(nil)

func (s *Instance) ID() string { return s.id }

func (s *Instance) path(suffix string) string { return "/sandboxes/" + url.PathEscape(s.id) + suffix }

func (s *Instance) Healthy(ctx context.Context) bool {
	resp, err := s.b.do(ctx, "GET", s.path(""), nil, nil, nil)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

func (s *Instance) Gone(ctx context.Context) bool {
	resp, err := s.b.do(ctx, "GET", s.path(""), nil, nil, nil)
	if err != nil {
		return sandbox.IsGone(err) // only a definitive 404; a timeout is not "gone"
	}
	resp.Body.Close()
	return false
}

func (s *Instance) Renew(ctx context.Context, d time.Duration) error {
	resp, err := s.b.do(ctx, "POST", s.path("/renew"), nil, nil, map[string]any{"seconds": int(d.Seconds())})
	if err == nil {
		resp.Body.Close()
	}
	return err
}

// Exec runs a shell command and streams its output to the callbacks. A
// non-zero exit status is a result, not an error. The service must run the
// command with bash, kill its whole process group at Timeout or when the
// request is cancelled, and send output as it is produced.
func (s *Instance) Exec(ctx context.Context, r sandbox.ExecRequest) (sandbox.ExecResult, error) {
	resp, err := s.b.do(ctx, "POST", s.path("/exec"), nil, nil, map[string]any{
		"command": r.Command, "cwd": r.Cwd, "timeout_ms": r.Timeout.Milliseconds(), "env": r.Env})
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var ev struct {
			O, E string
			Exit *int
		}
		if err := dec.Decode(&ev); err != nil {
			if ctx.Err() != nil {
				return sandbox.ExecResult{}, ctx.Err()
			}
			return sandbox.ExecResult{}, fmt.Errorf("exec stream ended without an exit status: %w", err)
		}
		switch {
		case ev.O != "" && r.OnStdout != nil:
			r.OnStdout(ev.O)
		case ev.E != "" && r.OnStderr != nil:
			r.OnStderr(ev.E)
		case ev.Exit != nil:
			return sandbox.ExecResult{ExitCode: *ev.Exit}, nil
		}
	}
}

func (s *Instance) ReadFile(ctx context.Context, p string, max int64) ([]byte, int64, error) {
	resp, err := s.b.do(ctx, "GET", s.path("/file"), url.Values{"path": {p}, "max": {strconv.FormatInt(max, 10)}}, nil, nil)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	size, _ := strconv.ParseInt(resp.Header.Get("X-File-Size"), 10, 64)
	return data, size, err
}

func (s *Instance) WriteFile(ctx context.Context, p string, data []byte) error {
	resp, err := s.b.do(ctx, "PUT", s.path("/file"), url.Values{"path": {p}}, bytes.NewReader(data), nil)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func (s *Instance) MkdirAll(ctx context.Context, p string) error {
	resp, err := s.b.do(ctx, "POST", s.path("/mkdir"), url.Values{"path": {p}}, nil, nil)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

type wireInfo struct {
	Path, Type string
	Size       int64
	Mtime      time.Time
}

func (w wireInfo) info() sandbox.FileInfo {
	return sandbox.FileInfo{Path: w.Path, Type: w.Type, Size: w.Size, ModifiedAt: w.Mtime}
}

func (s *Instance) List(ctx context.Context, p string, depth int) ([]sandbox.FileInfo, error) {
	resp, err := s.b.do(ctx, "GET", s.path("/list"), url.Values{"path": {p}, "depth": {strconv.Itoa(depth)}}, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var raw []wireInfo
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]sandbox.FileInfo, len(raw))
	for i, w := range raw {
		out[i] = w.info()
	}
	return out, nil
}

func (s *Instance) Stat(ctx context.Context, p string) (sandbox.FileInfo, error) {
	resp, err := s.b.do(ctx, "GET", s.path("/stat"), url.Values{"path": {p}}, nil, nil)
	if err != nil {
		return sandbox.FileInfo{}, err
	}
	defer resp.Body.Close()
	var w wireInfo
	if err := json.NewDecoder(resp.Body).Decode(&w); err != nil {
		return sandbox.FileInfo{}, err
	}
	return w.info(), nil
}
