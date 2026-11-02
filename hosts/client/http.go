package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/7ar-chat/harness/core/agui"
)

// HTTPOption configures HTTP.
type HTTPOption func(*httpClient)

// WithHTTPClient sets the http.Client used for requests (default: one without
// an overall timeout, as event streams are long-lived). Its Timeout must be zero.
func WithHTTPClient(c *http.Client) HTTPOption { return func(h *httpClient) { h.hc = c } }

// WithBackoff sets the wait between reconnections of an event stream: it
// starts at min and doubles up to max (defaults 200 ms and 5 s).
func WithBackoff(min, max time.Duration) HTTPOption {
	return func(h *httpClient) { h.minBackoff, h.maxBackoff = min, max }
}

// WithMaxReconnects sets how many consecutive failed reconnections an event
// stream tolerates before it gives up (default 8). A reconnection that delivers
// an event resets the count.
func WithMaxReconnects(n int) HTTPOption { return func(h *httpClient) { h.maxReconnects = n } }

type httpClient struct {
	base          string
	token         string
	hc            *http.Client
	minBackoff    time.Duration
	maxBackoff    time.Duration
	maxReconnects int
}

// HTTP returns a Client for the API of a harness-web server at baseURL
// (for example "http://127.0.0.1:8080"). token is the bearer token the server
// requires ("" for none).
func HTTP(baseURL, token string, opts ...HTTPOption) Client {
	c := &httpClient{base: strings.TrimRight(baseURL, "/"), token: token, hc: &http.Client{},
		minBackoff: 200 * time.Millisecond, maxBackoff: 5 * time.Second, maxReconnects: 8}
	for _, o := range opts {
		o(c)
	}
	return c
}

// do sends a request; a non-2xx answer is returned as the API error. The
// caller closes the body.
func (c *httpClient) do(ctx context.Context, method, p string, q url.Values, body any, hdr http.Header) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	u := c.base + "/api" + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, decodeError(resp)
	}
	return resp, nil
}

// decodeError reads the {"error":{"code","message"}} envelope of a failed answer.
func decodeError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var env struct {
		Error Error `json:"error"`
	}
	if json.Unmarshal(b, &env) == nil && env.Error.Code != "" {
		e := env.Error
		e.Status = resp.StatusCode
		return &e
	}
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return &Error{Status: resp.StatusCode, Code: "http_" + strconv.Itoa(resp.StatusCode), Message: msg}
}

// call performs a request and decodes the JSON answer into out (nil to ignore it).
func (c *httpClient) call(ctx context.Context, method, p string, q url.Values, body, out any) error {
	resp, err := c.do(ctx, method, p, q, body, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func limitQuery(n int) url.Values {
	q := url.Values{}
	if n > 0 {
		q.Set("limit", strconv.Itoa(n))
	}
	return q
}

func (c *httpClient) Models(ctx context.Context) (m ModelList, err error) {
	err = c.call(ctx, "GET", "/models", nil, nil, &m)
	return m, err
}

func (c *httpClient) CreateThread(ctx context.Context, title string) (*Thread, error) {
	var t Thread
	if err := c.call(ctx, "POST", "/threads", nil, map[string]string{"title": title}, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (c *httpClient) ListThreads(ctx context.Context, limit int) ([]Thread, error) {
	var out []Thread
	err := c.call(ctx, "GET", "/threads", limitQuery(limit), nil, &out)
	return out, err
}

func (c *httpClient) GetThread(ctx context.Context, id string) (*Thread, error) {
	var t Thread
	if err := c.call(ctx, "GET", "/threads/"+url.PathEscape(id), nil, nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (c *httpClient) RenameThread(ctx context.Context, id string, p ThreadPatch) (*Thread, error) {
	var t Thread
	if err := c.call(ctx, "PATCH", "/threads/"+url.PathEscape(id), nil, p, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (c *httpClient) DeleteThread(ctx context.Context, id string) error {
	return c.call(ctx, "DELETE", "/threads/"+url.PathEscape(id), nil, nil, nil)
}

func (c *httpClient) Send(ctx context.Context, threadID, text string, o SendOptions) (*Run, error) {
	var r Run
	if err := c.call(ctx, "POST", "/threads/"+url.PathEscape(threadID)+"/runs", nil, RunRequest{Message: text, SendOptions: o}, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *httpClient) GetRun(ctx context.Context, runID string) (*Run, error) {
	var r Run
	if err := c.call(ctx, "GET", "/runs/"+url.PathEscape(runID), nil, nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *httpClient) Steer(ctx context.Context, runID, text string) error {
	return c.call(ctx, "POST", "/runs/"+url.PathEscape(runID)+"/steer", nil, map[string]string{"text": text}, nil)
}

func (c *httpClient) Cancel(ctx context.Context, runID string) error {
	return c.call(ctx, "POST", "/runs/"+url.PathEscape(runID)+"/cancel", nil, nil, nil)
}

func (c *httpClient) Resume(ctx context.Context, threadID string, r ResumeRequest) (*Run, error) {
	var run Run
	if err := c.call(ctx, "POST", "/threads/"+url.PathEscape(threadID)+"/resume", nil, r, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func (c *httpClient) ListFiles(ctx context.Context, threadID, p string) ([]File, error) {
	q := url.Values{}
	if p != "" {
		q.Set("path", p)
	}
	var out []File
	err := c.call(ctx, "GET", "/threads/"+url.PathEscape(threadID)+"/files", q, nil, &out)
	return out, err
}

func (c *httpClient) DownloadFile(ctx context.Context, threadID, p string) (*Download, error) {
	resp, err := c.do(ctx, "GET", "/threads/"+url.PathEscape(threadID)+"/files/download", url.Values{"path": {p}}, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &Download{Path: p, Name: path.Base(p), Data: data}, nil
}

func (c *httpClient) Sessions(ctx context.Context, limit int) ([]Session, error) {
	var out []Session
	err := c.call(ctx, "GET", "/sessions", limitQuery(limit), nil, &out)
	return out, err
}

// Events opens the run's SSE stream. The first connection is made before
// returning, so that an unknown run or a bad token is an error here; later
// disconnections are resumed with Last-Event-ID.
func (c *httpClient) Events(ctx context.Context, runID string, after int64) (<-chan Event, error) {
	body, err := c.openStream(ctx, runID, after)
	if err != nil {
		return nil, err
	}
	out := make(chan Event)
	go c.pump(ctx, runID, after, body, out)
	return out, nil
}

func (c *httpClient) openStream(ctx context.Context, runID string, after int64) (io.ReadCloser, error) {
	hdr := http.Header{"Accept": {"text/event-stream"}}
	if after > 0 {
		hdr.Set("Last-Event-ID", strconv.FormatInt(after, 10))
	}
	resp, err := c.do(ctx, "GET", "/runs/"+url.PathEscape(runID)+"/events", nil, nil, hdr)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// pump forwards the stream's events to out, reconnecting from the last seq
// until a terminal event, ctx is done or the connection cannot be recovered.
func (c *httpClient) pump(ctx context.Context, runID string, after int64, body io.ReadCloser, out chan<- Event) {
	defer close(out)
	last := after
	failures := 0
	wait := c.minBackoff
	for {
		terminal, got := readSSE(ctx, body, &last, out)
		body.Close()
		if terminal || ctx.Err() != nil {
			return
		}
		if got {
			failures, wait = 0, c.minBackoff
		}
		for {
			failures++
			if failures > c.maxReconnects {
				lost(ctx, out, fmt.Errorf("the event stream of run %s was lost after %d reconnection attempts", runID, c.maxReconnects))
				return
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}
			wait = min(wait*2, c.maxBackoff)
			b, err := c.openStream(ctx, runID, last)
			if err == nil {
				body = b
				break
			}
			var api *Error
			if errors.As(err, &api) && api.Status >= 400 && api.Status < 500 && api.Status != 429 {
				lost(ctx, out, err)
				return
			}
		}
	}
}

// lost ends a stream with a synthetic RUN_ERROR so that a consumer waiting for a terminal event stops.
func lost(ctx context.Context, out chan<- Event, cause error) {
	data, _ := json.Marshal(map[string]any{"type": agui.RunError, "code": "stream_lost", "message": cause.Error()})
	select {
	case out <- Event{Type: agui.RunError, Data: data}:
	case <-ctx.Done():
	}
}

// readSSE parses the stream and sends each event whose seq is above *last,
// updating it. terminal reports that a terminal event was delivered; got that
// at least one event was.
func readSSE(ctx context.Context, body io.Reader, last *int64, out chan<- Event) (terminal, got bool) {
	r := bufio.NewReaderSize(body, 64<<10)
	var id string
	var data []string
	for {
		line, err := r.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "" && len(data) > 0:
			ev := Event{Data: json.RawMessage(strings.Join(data, "\n"))}
			seq, _ := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
			id, data = "", nil
			if seq > 0 && seq <= *last { // already delivered before a reconnection
				break
			}
			var head struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(ev.Data, &head)
			ev.Seq, ev.Type = seq, head.Type
			select {
			case out <- ev:
			case <-ctx.Done():
				return false, got
			}
			got = true
			if seq > 0 {
				*last = seq
			}
			if agui.IsTerminal(ev.Type) {
				return true, got
			}
		case strings.HasPrefix(line, "id:"):
			id = strings.TrimPrefix(strings.TrimPrefix(line, "id:"), " ")
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		if err != nil {
			return false, got
		}
	}
}
