package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

// OpenSandboxOptions configures the OpenSandbox backend.
type OpenSandboxOptions struct {
	URL             string
	APIKey          string
	UseServerProxy  bool
	Image           string
	CPU             string
	Memory          string
	TimeoutSeconds  int
	EgressAllowlist []string
	// EgressDenyCIDRs are denied in unrestricted mode (no allowlist).
	EgressDenyCIDRs []string
}

// OpenSandbox is the Backend for the official OpenSandbox server.
type OpenSandbox struct {
	conn opensandbox.ConnectionConfig
	opts OpenSandboxOptions
}

// NewOpenSandbox parses OPENSANDBOX_URL into protocol + domain.
func NewOpenSandbox(o OpenSandboxOptions) (*OpenSandbox, error) {
	u, err := url.Parse(strings.TrimRight(o.URL, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid OPENSANDBOX_URL %q", o.URL)
	}
	domain := u.Host
	if p := strings.Trim(u.Path, "/"); p != "" {
		domain += "/" + p
	}
	retry := opensandbox.DefaultRetryConfig()
	conn := opensandbox.ConnectionConfig{
		Domain:         domain,
		Protocol:       u.Scheme,
		APIKey:         o.APIKey,
		UseServerProxy: o.UseServerProxy,
		RequestTimeout: 60 * time.Second,
		HTTPClient:     &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()},
		Retry:          &retry,
		DisableMetrics: true,
	}
	return &OpenSandbox{conn: conn, opts: o}, nil
}

var _ Backend = (*OpenSandbox)(nil)

func isNotFound(err error) bool {
	var apiErr *opensandbox.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

var _ Instance = (*osbInstance)(nil)

// opErr classifies an execd error. The provider's SANDBOX_NOT_FOUND code means
// the sandbox itself is gone; another 404 is the sandbox gone when the lifecycle
// API confirms it (the definitive 404 rule of Connect), else (file
// operations) a missing file. Gone sandboxes yield ErrGone (see IsGone).
func (s *osbInstance) opErr(ctx context.Context, err error, file bool) error {
	switch {
	case err == nil:
		return nil
	case IsGone(err):
		return fmt.Errorf("%w: %w", ErrGone, err)
	case !isNotFound(err):
		return err
	}
	gctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if s.Gone(gctx) {
		return fmt.Errorf("%w: %w", ErrGone, err)
	}
	if file {
		return ErrNotFound
	}
	return err
}

// DefaultEgressDenyCIDRs isolate a sandbox in unrestricted mode from other
// sandboxes (their execd is unauthenticated), the host, the proxy and metadata:
// RFC 1918, link-local, CGNAT (100.64/10, sometimes pod/VPC ranges), "this
// network" (0/8), IPv6 unique-local (fc00::/7, incl. fd00:ec2::254 metadata)
// and IPv6 link-local, and IPv4-mapped IPv6 (::ffff:0:0/96: ::ffff:10.0.0.1 is
// 10.0.0.1 on a dual-stack socket). Same list as the SANDBOX_EGRESS_DENY_CIDRS default.
var DefaultEgressDenyCIDRs = []string{"172.16.0.0/12", "10.0.0.0/8", "192.168.0.0/16", "169.254.0.0/16",
	"100.64.0.0/10", "0.0.0.0/8", "fc00::/7", "fe80::/10", "::ffff:0:0/96"}

// EgressPolicy is the network policy of every new sandbox (never nil: without a
// policy a sandbox can reach other sandboxes and the host).
//   - allowlist mode: deny by default, allow the listed domains;
//   - unrestricted mode: allow by default but deny private/link-local CIDRs
//     (a CIDR deny beats a domain allow, so internal hosts are unreachable).
func EgressPolicy(o OpenSandboxOptions) *opensandbox.NetworkPolicy {
	if len(o.EgressAllowlist) == 0 {
		cidrs := o.EgressDenyCIDRs
		if len(cidrs) == 0 {
			cidrs = DefaultEgressDenyCIDRs
		}
		policy := &opensandbox.NetworkPolicy{DefaultAction: "allow"}
		for _, c := range cidrs {
			policy.Egress = append(policy.Egress, opensandbox.NetworkRule{Action: "deny", Target: c})
		}
		return policy
	}
	policy := &opensandbox.NetworkPolicy{DefaultAction: "deny"}
	for _, d := range o.EgressAllowlist {
		policy.Egress = append(policy.Egress, opensandbox.NetworkRule{Action: "allow", Target: d})
	}
	return policy
}

func (b *OpenSandbox) Create(ctx context.Context, co CreateOptions) (Instance, error) {
	timeout := b.opts.TimeoutSeconds
	opts := opensandbox.SandboxCreateOptions{
		Image:          b.opts.Image,
		ResourceLimits: opensandbox.ResourceLimits{"cpu": b.opts.CPU, "memory": b.opts.Memory},
		TimeoutSeconds: &timeout,
		Metadata:       co.Metadata,
		ReadyTimeout:   2 * time.Minute,
	}
	opts.NetworkPolicy = EgressPolicy(b.opts)
	sb, err := opensandbox.CreateSandbox(ctx, b.conn, opts)
	if err != nil {
		return nil, err
	}
	return &osbInstance{sb: sb, conn: b.conn}, nil
}

func (b *OpenSandbox) Connect(ctx context.Context, id string) (Instance, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	sb, err := opensandbox.ConnectSandbox(cctx, b.conn, id)
	if err != nil {
		// Only a definitive answer from the lifecycle API means "gone"; timeouts,
		// cancellations and 5xx are transient and must keep the workspace.
		if isNotFound(err) || b.gone(ctx, id) {
			return nil, ErrNoSandbox
		}
		return nil, err
	}
	return &osbInstance{sb: sb, conn: b.conn}, nil
}

// gone asks the lifecycle API whether the sandbox is definitively dead.
func (b *OpenSandbox) gone(ctx context.Context, id string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	info, err := opensandbox.NewSandboxManager(b.conn).GetSandboxInfo(ctx, id)
	if err != nil {
		return isNotFound(err)
	}
	return info.Status.State == opensandbox.StateTerminated || info.Status.State == opensandbox.StateFailed
}

func (b *OpenSandbox) Kill(ctx context.Context, id string) error {
	err := opensandbox.NewSandboxManager(b.conn).KillSandbox(ctx, id)
	if err != nil && isNotFound(err) {
		return nil
	}
	return err
}

// execdAccessTokenHeader mirrors the SDK's unexported execdAuthHeader.
const execdAccessTokenHeader = "X-EXECD-ACCESS-TOKEN"

type osbInstance struct {
	sb   *opensandbox.Sandbox
	conn opensandbox.ConnectionConfig
}

func (s *osbInstance) ID() string { return s.sb.ID() }

func (s *osbInstance) Healthy(ctx context.Context) bool {
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.sb.IsHealthy(hctx)
}

func (s *osbInstance) Gone(ctx context.Context) bool { // bounded by the caller's context
	info, err := s.sb.GetInfo(ctx)
	if err != nil {
		return isNotFound(err)
	}
	return info.Status.State == opensandbox.StateTerminated || info.Status.State == opensandbox.StateFailed
}

func (s *osbInstance) Renew(ctx context.Context, d time.Duration) error {
	_, err := s.sb.Renew(ctx, d)
	return err
}

func withNewline(t string) string {
	if strings.HasSuffix(t, "\n") {
		return t
	}
	return t + "\n"
}

func (s *osbInstance) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	var execID string
	handlers := &opensandbox.ExecutionHandlers{
		SkipAccumulation: true,
		OnInit: func(i opensandbox.ExecutionInit) error {
			execID = i.ID
			return nil
		},
		OnStdout: func(m opensandbox.OutputMessage) error {
			if req.OnStdout != nil {
				req.OnStdout(withNewline(m.Text))
			}
			return nil
		},
		OnStderr: func(m opensandbox.OutputMessage) error {
			if req.OnStderr != nil {
				req.OnStderr(withNewline(m.Text))
			}
			return nil
		},
	}
	exec, err := s.sb.RunCommandWithOpts(ctx, opensandbox.RunCommandRequest{
		Command: req.Command,
		Cwd:     req.Cwd,
		Timeout: req.Timeout.Milliseconds(),
		Envs:    req.Env,
	}, handlers)
	if ctx.Err() != nil {
		if execID != "" {
			go s.interrupt(execID)
		}
		return ExecResult{}, ctx.Err()
	}
	if err != nil {
		return ExecResult{}, s.opErr(ctx, err, false)
	}
	if exec.ExitCode != nil {
		return ExecResult{ExitCode: *exec.ExitCode}, nil
	}
	if exec.Error != nil {
		if code, perr := strconv.Atoi(strings.TrimSpace(exec.Error.Value)); perr == nil {
			return ExecResult{ExitCode: code}, nil
		}
		return ExecResult{}, fmt.Errorf("%s: %s", exec.Error.Name, exec.Error.Value)
	}
	return ExecResult{ExitCode: 0}, nil
}

// interrupt stops a running command (best effort; the SDK Sandbox does not expose it).
func (s *osbInstance) interrupt(execID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ep, err := s.sb.GetEndpoint(ctx, opensandbox.DefaultExecdPort)
	if err != nil {
		return
	}
	endpoint := s.conn.RewriteEndpointURL(ep.Endpoint)
	if !strings.HasPrefix(endpoint, "http") {
		endpoint = s.conn.GetProtocol() + "://" + endpoint
	}
	headers := map[string]string{}
	for k, v := range ep.Headers {
		headers[k] = v
	}
	// Same as the SDK's resolveExecd: in server-proxy mode the API key doubles as the execd token.
	if s.conn.UseServerProxy && s.conn.GetAPIKey() != "" {
		if _, ok := headers[execdAccessTokenHeader]; !ok {
			headers[execdAccessTokenHeader] = s.conn.GetAPIKey()
		}
	}
	opts := []opensandbox.Option{opensandbox.WithHeaders(headers)}
	if s.conn.HTTPClient != nil {
		opts = append(opts, opensandbox.WithHTTPClient(s.conn.HTTPClient))
	}
	client := opensandbox.NewExecdClient(endpoint, "", opts...)
	if err := client.InterruptCommand(ctx, execID); err != nil {
		slog.Debug("sandbox interrupt failed", "sandbox_id", s.ID(), "error", err)
	}
}

func (s *osbInstance) Stat(ctx context.Context, p string) (FileInfo, error) {
	infos, err := s.sb.GetFileInfo(ctx, p)
	if err != nil {
		return FileInfo{}, s.opErr(ctx, err, true)
	}
	if fi, ok := infos[p]; ok {
		return toInfo(fi, p), nil
	}
	for k, fi := range infos {
		return toInfo(fi, k), nil
	}
	return FileInfo{}, ErrNotFound
}

func toInfo(fi opensandbox.FileInfo, fallback string) FileInfo {
	p := fi.Path
	if p == "" {
		p = fallback
	}
	t := "file"
	if fi.Type == "directory" || fi.Type == "dir" {
		t = "directory"
	}
	return FileInfo{Path: p, Type: t, Size: fi.Size, ModifiedAt: fi.ModifiedAt}
}

func (s *osbInstance) ReadFile(ctx context.Context, p string, maxBytes int64) ([]byte, int64, error) {
	info, err := s.Stat(ctx, p)
	if err != nil {
		return nil, 0, err
	}
	if info.Type == "directory" {
		return nil, 0, fmt.Errorf("%s is a directory", p)
	}
	rangeHeader := ""
	if maxBytes > 0 && info.Size > maxBytes {
		rangeHeader = fmt.Sprintf("bytes=0-%d", maxBytes-1)
	}
	rc, err := s.sb.DownloadFile(ctx, p, rangeHeader)
	if err != nil {
		return nil, 0, s.opErr(ctx, err, true)
	}
	defer rc.Close()
	var r io.Reader = rc
	if maxBytes > 0 {
		r = io.LimitReader(rc, maxBytes)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, 0, err
	}
	size := info.Size
	if size < int64(len(data)) {
		size = int64(len(data))
	}
	return data, size, nil
}

func (s *osbInstance) MkdirAll(ctx context.Context, p string) error {
	return s.opErr(ctx, s.sb.CreateDirectory(ctx, p, 755), false)
}

func (s *osbInstance) WriteFile(ctx context.Context, p string, data []byte) error {
	if dir := path.Dir(p); dir != "/" && dir != "." {
		if err := s.MkdirAll(ctx, dir); err != nil {
			return fmt.Errorf("create parent directory: %w", err)
		}
	}
	return s.opErr(ctx, s.sb.UploadFile(ctx, bytes.NewReader(data), opensandbox.UploadFileOptions{
		FileName: path.Base(p),
		Metadata: opensandbox.FileMetadata{Path: p, Mode: 644},
	}), false)
}

func (s *osbInstance) List(ctx context.Context, p string, depth int) ([]FileInfo, error) {
	infos, err := s.sb.ListDirectoryWithDepth(ctx, p, depth)
	if err != nil {
		return nil, s.opErr(ctx, err, true)
	}
	out := make([]FileInfo, 0, len(infos))
	for _, fi := range infos {
		if path.Clean(fi.Path) == path.Clean(p) {
			continue
		}
		out = append(out, toInfo(fi, ""))
	}
	return out, nil
}
