// Package e2b is the sandbox.Backend for E2B (https://e2b.dev): one Firecracker micro-VM per conversation, created
// through E2B's REST API and driven through the envd agent inside it (Connect-protocol RPCs for commands and
// directories, plain HTTP for file contents). It uses only the standard library: E2B publishes no Go SDK.
//
// Selected with SANDBOX_BACKEND=e2b and E2B_API_KEY (or `harness auth login e2b`).
//
// The default template ("base") has bash, GNU coreutils, python3 and git but no ripgrep; search_files falls back to
// python3. Build a template from sandbox-image/Dockerfile for the full toolchain (E2B_TEMPLATE).
package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/7ar-chat/harness/adapters/sandbox/backends"
	"github.com/7ar-chat/harness/core/sandbox"
)

// DefaultTemplate is the public E2B template sandboxes start from.
const DefaultTemplate = "base"

// DefaultDomain is E2B's cloud domain; the control plane is api.<domain>, a sandbox is <port>-<id>.<domain>.
const DefaultDomain = "e2b.app"

// envdPort is the port of the envd agent in every sandbox.
const envdPort = 49983

// Options configures the E2B backend.
type Options struct {
	APIKey string
	// Template is the template (or alias) to start from; "" means DefaultTemplate.
	Template string
	// Domain is the E2B domain of a self-hosted installation; "" means DefaultDomain.
	Domain string
	// APIURL overrides the control-plane URL (tests, proxies); "" means https://api.<Domain>.
	APIURL string
	// EnvdURL overrides the URL of the in-sandbox agent for every sandbox (tests); "" means https://49983-<id>.<Domain>.
	EnvdURL string
	// TimeoutSeconds is the lifetime of a sandbox before it is killed; Renew extends it.
	TimeoutSeconds int
	// EgressAllowlist, when set, makes egress deny-by-default with these domains allowed.
	EgressAllowlist []string
	// EgressDenyCIDRs are denied in unrestricted mode (no allowlist); empty: DefaultEgressDenyCIDRs.
	EgressDenyCIDRs []string
	// Images maps a sandbox profile to the template it starts from; a profile without an entry uses Template.
	Images map[string]string
	// HTTPClient is used for every request; nil means a client with sensible transport timeouts and no overall deadline.
	HTTPClient *http.Client
}

// E2B is the sandbox.Backend for E2B.
type E2B struct {
	opts   Options
	apiURL string
	hc     *http.Client
}

var _ sandbox.Backend = (*E2B)(nil)

// New builds the backend; it contacts nothing until the first Create.
func New(o Options) (*E2B, error) {
	if strings.TrimSpace(o.APIKey) == "" {
		return nil, errors.New("e2b: an API key is required")
	}
	if o.Template == "" {
		o.Template = DefaultTemplate
	}
	if o.Domain == "" {
		o.Domain = DefaultDomain
	}
	api := strings.TrimRight(o.APIURL, "/")
	if api == "" {
		api = "https://api." + o.Domain
	}
	if _, err := url.Parse(api); err != nil {
		return nil, fmt.Errorf("e2b: invalid API URL: %w", err)
	}
	hc := o.HTTPClient
	if hc == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.ResponseHeaderTimeout = 5 * time.Minute // commands stream for as long as they run; headers arrive at once
		hc = &http.Client{Transport: tr}
	}
	return &E2B{opts: o, apiURL: api, hc: hc}, nil
}

// sandboxInfo is the part of E2B's Sandbox object the backend uses.
type sandboxInfo struct {
	SandboxID       string `json:"sandboxID"`
	Domain          string `json:"domain"`
	EnvdAccessToken string `json:"envdAccessToken"`
	// TrafficAccessToken is the token a request to a port of the sandbox must carry when the sandbox does not allow
	// public traffic.
	TrafficAccessToken string `json:"trafficAccessToken"`
	State           string `json:"state"`
}

// templateFor is the template of a profile: its entry of Options.Images, else Options.Template.
func (b *E2B) templateFor(profile string) string {
	if t := backends.ImageFor(b.opts.Images, profile); t != "" {
		return t
	}
	return b.opts.Template
}

func (b *E2B) Create(ctx context.Context, co sandbox.CreateOptions) (sandbox.Instance, error) {
	body := map[string]any{
		"templateID": b.templateFor(co.Profile),
		"timeout":    max(b.opts.TimeoutSeconds, 60),
		"secure":     true,
	}
	if len(co.Metadata) > 0 {
		body["metadata"] = co.Metadata
	}
	if n := networkConfig(b.opts); n != nil {
		// A browser sandbox serves its tools and its live view on ports: only requests that carry the sandbox's traffic
		// token (see Endpoint) may reach them.
		if strings.EqualFold(strings.TrimSpace(co.Profile), "browser") {
			n["allowPublicTraffic"] = false
		}
		body["network"] = n
	}
	var info sandboxInfo
	if err := b.api(ctx, http.MethodPost, "/sandboxes", body, &info); err != nil {
		return nil, err
	}
	if info.SandboxID == "" {
		return nil, errors.New("e2b: the API returned no sandbox ID")
	}
	inst := b.instance(info)
	if err := inst.bootstrap(ctx); err != nil {
		_ = b.Kill(context.WithoutCancel(ctx), info.SandboxID) // do not leave a half-prepared sandbox running
		return nil, fmt.Errorf("e2b: prepare %s: %w", sandbox.Workspace, err)
	}
	return inst, nil
}

func (b *E2B) Connect(ctx context.Context, id string) (sandbox.Instance, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var info sandboxInfo
	// POST /connect returns the access token and resumes a paused sandbox; a missing one is a definitive 404.
	err := b.api(cctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/connect", map[string]any{"timeout": max(b.opts.TimeoutSeconds, 60)}, &info)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return nil, sandbox.ErrNoSandbox
		}
		return nil, err
	}
	if info.SandboxID == "" {
		info.SandboxID = id
	}
	return b.instance(info), nil
}

func (b *E2B) Kill(ctx context.Context, id string) error {
	err := b.api(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(id), nil, nil)
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}

func (b *E2B) instance(info sandboxInfo) *instance {
	domain := info.Domain
	if domain == "" {
		domain = b.opts.Domain
	}
	base := b.opts.EnvdURL
	if base == "" {
		base = fmt.Sprintf("https://%d-%s.%s", envdPort, info.SandboxID, domain)
	}
	return &instance{b: b, id: info.SandboxID, token: info.EnvdAccessToken, traffic: info.TrafficAccessToken,
		domain: domain, envd: strings.TrimRight(base, "/")}
}

// errNotFound is a 404 from the control plane: the sandbox (or template) does not exist.
var errNotFound = errors.New("e2b: not found")

// apiError is a non-2xx answer of the control plane.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("e2b API: HTTP %d: %s", e.Status, e.Message) }

// api calls the control plane with the API key; out, when not nil, receives the JSON answer.
func (b *E2B) api(ctx context.Context, method, path string, in, out any) error {
	var rd io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.apiURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", b.opts.APIKey)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", sandbox.ErrUnavailable, scrub(err, b.opts.APIKey))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	case resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(apiMessage(data)), "invalid sandbox id"):
		return errNotFound // an ID that cannot exist is a definitive absence
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("%w: %w", sandbox.ErrQuota, &apiError{resp.StatusCode, apiMessage(data)})
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: %w", sandbox.ErrUnavailable, &apiError{resp.StatusCode, apiMessage(data)})
	case resp.StatusCode >= 300:
		return &apiError{resp.StatusCode, apiMessage(data)}
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("e2b API: unreadable answer: %w", err)
		}
	}
	return nil
}

// apiMessage is the message of an E2B error body ({"code":..,"message":..}), else a clipped body.
func apiMessage(data []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &e) == nil && e.Message != "" {
		return e.Message
	}
	s := strings.TrimSpace(string(data))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// scrub keeps a secret out of an error (a transport error can quote the request URL or headers).
func scrub(err error, secret string) error {
	if secret == "" || !strings.Contains(err.Error(), secret) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, "****"))
}
