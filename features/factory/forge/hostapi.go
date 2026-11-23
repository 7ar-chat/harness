package forge

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

	"github.com/7ar-chat/harness/core/githost"
)

// ErrNoAPI is returned for a host that has no pull request API (plain git over HTTPS or SSH): the branch can be
// pushed, but a person has to open the change.
var ErrNoAPI = errors.New("this git host has no pull request API")

// hostClient is the REST client of one registered host (GitLab or Gitea). It sends the token of the user on whose
// behalf the call is made (githost.LoginFrom), never follows a redirect to another host (a custom auth header
// would travel with it), and masks the token in every error.
type hostClient struct {
	provider string
	host     githost.Host
	res      githost.Resolver
	hc       *http.Client
	ua       string
	// header names the auth header the provider reads ("PRIVATE-TOKEN: t", "Authorization: token t").
	header func(tok string) (name, value string)
}

func newHostClient(provider string, h githost.Host, res githost.Resolver, hc *http.Client, header func(string) (string, string)) *hostClient {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	base := *hc
	base.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
			return fmt.Errorf("refusing a redirect to %s", req.URL.Host)
		}
		return nil
	}
	return &hostClient{provider: provider, host: h, res: res, hc: &base, ua: "harness-factory", header: header}
}

// token is the token to send for this call.
func (c *hostClient) token(ctx context.Context) (string, error) {
	if c.res == nil {
		return "", githost.ErrNoCredential
	}
	cred, err := c.res.Credential(ctx, githost.LoginFrom(ctx), c.host.Name)
	if err != nil {
		if errors.Is(err, githost.ErrNoCredential) {
			return "", fmt.Errorf("no access token for %s: add a personal access token in Profile, Git access", c.host.Name)
		}
		return "", err
	}
	if cred.Token == "" {
		return "", fmt.Errorf("no access token for %s: add a personal access token in Profile, Git access", c.host.Name)
	}
	return cred.Token, nil
}

// projectPath validates repo ("host/path") as one on this host and returns the path.
func (c *hostClient) projectPath(repo string) (string, error) {
	h, p, ok := githost.SplitRepo(repo)
	if !ok || h != c.host.Name {
		return "", fmt.Errorf("%s: %q is not a repository of %s", c.provider, repo, c.host.Name)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("%s: invalid repository %q", c.provider, repo)
		}
	}
	return p, nil
}

type hostRequest struct {
	method string
	path   string // already escaped
	query  url.Values
	body   any
	accept string
	// limit caps the body read (0: maxBody); tail keeps the end of a longer body instead of failing.
	limit int64
	tail  bool
}

// do sends r and returns the body of a 2xx answer; any other status is an *APIError.
func (c *hostClient) do(ctx context.Context, r hostRequest) ([]byte, error) {
	tok, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	var payload io.Reader
	if r.body != nil {
		b, err := json.Marshal(r.body)
		if err != nil {
			return nil, fmt.Errorf("%s: encode %s %s: %w", c.provider, r.method, r.path, err)
		}
		payload = bytes.NewReader(b)
	}
	u := c.host.APIBase() + r.path
	if len(r.query) > 0 {
		u += "?" + r.query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, r.method, u, payload)
	if err != nil {
		return nil, fmt.Errorf("%s: %s %s: %w", c.provider, r.method, r.path, err)
	}
	req.Header.Set("User-Agent", c.ua)
	accept := r.accept
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	k, v := c.header(tok)
	req.Header.Set(k, v)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %s %s: %s", c.provider, r.method, r.path, Redact(err.Error(), tok))
	}
	defer resp.Body.Close()
	data, _, readErr := readBody(resp.Body, request{limit: r.limit, tail: r.tail})
	if readErr != nil {
		return nil, fmt.Errorf("%s: read %s %s: %w", c.provider, r.method, r.path, readErr)
	}
	if resp.StatusCode < 400 {
		return data, nil
	}
	return nil, &APIError{Provider: c.provider, Method: r.method, Path: r.path, Status: resp.StatusCode,
		Message: Redact(hostErrorMessage(data), tok), RateLimited: resp.StatusCode == http.StatusTooManyRequests}
}

func (c *hostClient) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	data, err := c.do(ctx, hostRequest{method: http.MethodGet, path: path, query: q})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s: decode %s: %w", c.provider, path, err)
	}
	return nil
}

// pages reads a paginated list: fn gets each page's body and returns how many items it held; the walk ends on a
// short page or after maxPages.
func (c *hostClient) pages(ctx context.Context, path string, q url.Values, sizeParam string, size int, fn func(page []byte) (int, error)) error {
	for page := 1; page <= maxPages; page++ {
		qq := url.Values{}
		for k, v := range q {
			qq[k] = v
		}
		qq.Set(sizeParam, fmt.Sprint(size))
		qq.Set("page", fmt.Sprint(page))
		data, err := c.do(ctx, hostRequest{method: http.MethodGet, path: path, query: qq})
		if err != nil {
			return err
		}
		n, err := fn(data)
		if err != nil {
			return err
		}
		if n < size {
			return nil
		}
	}
	return nil
}

// hostErrorMessage reads the message of a GitLab or Gitea error body.
func hostErrorMessage(data []byte) string {
	var e struct {
		Message          any    `json:"message"`
		Error            any    `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if json.Unmarshal(data, &e) != nil {
		return clipRunes(strings.TrimSpace(string(data)), 300)
	}
	for _, v := range []any{e.Message, e.Error, e.ErrorDescription} {
		switch t := v.(type) {
		case string:
			if t != "" {
				return clipRunes(t, 300)
			}
		case []any, map[string]any:
			b, _ := json.Marshal(t)
			return clipRunes(string(b), 300)
		}
	}
	return clipRunes(strings.TrimSpace(string(data)), 300)
}

// duplicatePR turns the "already exists" answer of a host (409) into the 422 the tools recover from.
func duplicatePR(err error) error {
	var ae *APIError
	if errors.As(err, &ae) && (ae.Status == http.StatusConflict || ae.Status == http.StatusUnprocessableEntity) &&
		strings.Contains(strings.ToLower(ae.Message), "already") {
		cp := *ae
		cp.Status = http.StatusUnprocessableEntity
		return &cp
	}
	return err
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

const draftPrefix = "Draft: "

func titleFor(title string, draft bool) string {
	if draft && !isDraftTitle(title) {
		return draftPrefix + title
	}
	return title
}

func isDraftTitle(t string) bool {
	l := strings.ToLower(strings.TrimSpace(t))
	return strings.HasPrefix(l, "draft:") || strings.HasPrefix(l, "wip:") || strings.HasPrefix(l, "[draft]") || strings.HasPrefix(l, "[wip]")
}

// noAPI is the forge of a host without a pull request API.
type noAPI struct{ host string }

func (n noAPI) err() error {
	return fmt.Errorf("%w (%s): push the branch and ask a person to open the change", ErrNoAPI, n.host)
}
