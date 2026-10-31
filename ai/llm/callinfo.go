package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CallInfo collects what the HTTP layer learns about one model call that eino
// does not surface: the provider's Retry-After hint of a failed call, the
// refusal text of a streamed answer (delta.refusal, dropped by eino-ext) and
// the stream's activity (every SSE event, also one that is not a chunk; not
// comment lines).
type CallInfo struct {
	mu         sync.Mutex
	retryAfter time.Duration
	refusal    strings.Builder
	onActivity func()
}

// OnActivity registers f, called whenever the call's stream shows life (an
// SSE event line, also of events the client does not forward as chunks; never
// a keep-alive comment), e.g. to reset a stall watchdog.
func (c *CallInfo) OnActivity(f func()) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.onActivity = f
	c.mu.Unlock()
}

// RecordActivity reports stream activity of the call in ctx (no-op without CallInfo).
func RecordActivity(ctx context.Context) {
	ci := callInfoFrom(ctx)
	if ci == nil {
		return
	}
	ci.mu.Lock()
	f := ci.onActivity
	ci.mu.Unlock()
	if f != nil {
		f()
	}
}

type callInfoKey struct{}

// WithCallInfo returns a context whose model calls record into the returned CallInfo.
func WithCallInfo(ctx context.Context) (context.Context, *CallInfo) {
	ci := &CallInfo{}
	return context.WithValue(ctx, callInfoKey{}, ci), ci
}

func callInfoFrom(ctx context.Context) *CallInfo {
	ci, _ := ctx.Value(callInfoKey{}).(*CallInfo)
	return ci
}

// RecordRetryAfter records the provider's retry hint of the call in ctx (no-op without CallInfo).
func RecordRetryAfter(ctx context.Context, d time.Duration) {
	if ci := callInfoFrom(ctx); ci != nil && d > 0 {
		ci.mu.Lock()
		ci.retryAfter = d
		ci.mu.Unlock()
	}
}

// RecordRefusal appends refusal text of the call in ctx (no-op without CallInfo).
func RecordRefusal(ctx context.Context, text string) {
	if ci := callInfoFrom(ctx); ci != nil && text != "" {
		ci.mu.Lock()
		ci.refusal.WriteString(text)
		ci.mu.Unlock()
	}
}

// RetryAfter is the provider's retry hint (0: none).
func (c *CallInfo) RetryAfter() time.Duration {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.retryAfter
}

// Refusal is the refusal text the provider streamed ("" when none).
func (c *CallInfo) Refusal() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refusal.String()
}

// observeTransport records Retry-After hints, streamed refusals and stream
// activity into the request context's CallInfo.
type observeTransport struct{ base http.RoundTripper }

func (t *observeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	ctx := req.Context()
	if callInfoFrom(ctx) == nil {
		return resp, nil
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		RecordRetryAfter(ctx, ParseRetryAfter(resp.Header, time.Now()))
	}
	if resp.StatusCode == http.StatusOK && strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		resp.Body = &refusalScanner{ReadCloser: resp.Body, ctx: ctx}
	}
	return resp, nil
}

// ParseRetryAfter reads retry-after-ms, Retry-After (seconds or HTTP date) and
// x-ratelimit-reset-requests/-tokens ("1s", "6m0s") headers (0: none).
func ParseRetryAfter(h http.Header, now time.Time) time.Duration {
	if ms, err := strconv.ParseFloat(strings.TrimSpace(h.Get("retry-after-ms")), 64); err == nil && ms > 0 {
		return time.Duration(ms * float64(time.Millisecond))
	}
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if s, err := strconv.ParseFloat(v, 64); err == nil && s > 0 {
			return time.Duration(s * float64(time.Second))
		}
		if at, err := http.ParseTime(v); err == nil && at.After(now) {
			return at.Sub(now)
		}
	}
	var best time.Duration
	for _, k := range []string{"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens"} {
		if d, err := time.ParseDuration(strings.TrimSpace(h.Get(k))); err == nil && d > best {
			best = d
		}
	}
	return best
}

// refusalScanner tees an SSE body: it reports the activity of its event lines and records the delta.refusal texts it carries.
type refusalScanner struct {
	io.ReadCloser
	ctx     context.Context
	pending []byte
}

var refusalField = []byte(`"refusal":"`)

func (r *refusalScanner) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.pending = append(r.pending, p[:n]...)
		for {
			i := bytes.IndexByte(r.pending, '\n')
			if i < 0 {
				break
			}
			r.line(r.pending[:i])
			r.pending = r.pending[i+1:]
		}
	}
	if err != nil && len(r.pending) > 0 {
		r.line(r.pending)
		r.pending = nil
	}
	return n, err
}

func (r *refusalScanner) line(line []byte) {
	line = bytes.TrimSpace(line)
	if bytes.HasPrefix(line, []byte("data:")) || bytes.HasPrefix(line, []byte("event:")) {
		// A provider event (also one the client does not forward): the call is
		// alive. Comment lines (": keep-alive") are not (round 3, N3).
		RecordActivity(r.ctx)
	}
	if !bytes.HasPrefix(line, []byte("data:")) || !bytes.Contains(bytes.ReplaceAll(line, []byte(" "), nil), refusalField) {
		return
	}
	var chunk struct {
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Refusal string `json:"refusal"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(bytes.TrimSpace(line[len("data:"):]), &chunk) != nil {
		return
	}
	for _, c := range chunk.Choices {
		if c.Index == 0 {
			RecordRefusal(r.ctx, c.Delta.Refusal)
		}
	}
}
