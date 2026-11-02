package llm_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm"
)

// roundTripFunc is an http.RoundTripper made of a function (a fake provider with no listener).
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

const chatAnswer = `{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}]}`

func TestSessionHeaderIsSentOnlyForCallsMadeUnderASession(t *testing.T) {
	// Options.SessionHeader carries the id given to WithSession on every call of that context; calls without a session, or with an empty id, send no such header.
	// Arrange
	var mu sync.Mutex
	var seen []http.Header
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		return jsonResponse(200, chatAnswer), nil
	})
	cfg := llm.Options{Provider: "openai", BaseURL: "http://gateway.test/v1", Models: []string{"m"}, SessionHeader: "x-session-id"}
	m, err := llm.NewWithTransport(cfg, base).Model(context.Background(), "m")
	if err != nil {
		t.Fatal(err)
	}
	ask := func(ctx context.Context) {
		t.Helper()
		if _, err := m.Generate(ctx, []*ai.Message{ai.UserMessage("ping")}); err != nil {
			t.Fatal(err)
		}
	}

	// Act
	ask(llm.WithSession(context.Background(), "thread-7"))
	ask(context.Background())
	ask(llm.WithSession(context.Background(), ""))

	// Assert
	got := []string{seen[0].Get("x-session-id"), seen[1].Get("x-session-id"), seen[2].Get("x-session-id")}
	if got[0] != "thread-7" || got[1] != "" || got[2] != "" {
		t.Fatalf("session headers %q", got)
	}
}

func TestSessionIsIgnoredWithoutAConfiguredHeader(t *testing.T) {
	// Without Options.SessionHeader the thread id is not leaked to the gateway under any name.
	// Arrange
	var seen http.Header
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = r.Header.Clone()
		return jsonResponse(200, chatAnswer), nil
	})
	cfg := llm.Options{Provider: "openai", BaseURL: "http://gateway.test/v1", Models: []string{"m"}}
	m, _ := llm.NewWithTransport(cfg, base).Model(context.Background(), "m")

	// Act
	_, err := m.Generate(llm.WithSession(context.Background(), "thread-7"), []*ai.Message{ai.UserMessage("ping")})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range seen {
		if strings.Contains(strings.Join(v, ","), "thread-7") {
			t.Fatalf("header %s leaked the session id: %v", k, v)
		}
	}
}

func TestSessionAndCallInfoApplyToResponsesAndAnthropicModelsToo(t *testing.T) {
	// Every wire protocol shares the provider's HTTP client: the session header reaches the Responses and Anthropic endpoints, which are addressed under their own paths with their own credentials.
	// Arrange
	type call struct{ path, session, auth, apiKey string }
	var mu sync.Mutex
	var calls []call
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		calls = append(calls, call{r.URL.String(), r.Header.Get("x-session-id"), r.Header.Get("Authorization"), r.Header.Get("x-api-key")})
		mu.Unlock()
		return jsonResponse(500, `{"error":{"message":"stop here"}}`), nil
	})
	ctx := llm.WithSession(context.Background(), "s-1")
	resp := llm.NewWithTransport(llm.Options{Provider: "openai", BaseURL: "http://gw.test/v1/", APIKey: "sk", Models: []string{"r"}, ResponsesModels: []string{" r "}, SessionHeader: "x-session-id"}, base)
	anth := llm.NewWithTransport(llm.Options{Provider: "anthropic", BaseURL: "http://anth.test", APIKey: "ak", Models: []string{"claude"}, SessionHeader: "x-session-id"}, base)
	rm, errR := resp.Model(ctx, "r")
	am, errA := anth.Model(ctx, "claude")
	if errR != nil || errA != nil {
		t.Fatal(errR, errA)
	}

	// Act
	_, _ = rm.Generate(ctx, []*ai.Message{ai.UserMessage("hi")})
	_, _ = am.Generate(ctx, []*ai.Message{ai.UserMessage("hi")})

	// Assert
	want := []call{
		{"http://gw.test/v1/responses", "s-1", "Bearer sk", ""},
		{"http://anth.test/v1/messages", "s-1", "", "ak"},
	}
	if len(calls) != 2 || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("calls %+v, want %+v", calls, want)
	}
}

func TestResponsesModelDefaultsToTheOpenAIEndpoint(t *testing.T) {
	// Without a BaseURL a Responses model calls api.openai.com/v1/responses.
	// Arrange
	var url string
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		url = r.URL.String()
		return jsonResponse(500, `{}`), nil
	})
	m, err := llm.NewWithTransport(llm.Options{Models: []string{"r"}, ResponsesModels: []string{"r"}}, base).Model(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}

	// Act
	_, _ = m.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")})

	// Assert
	if url != "https://api.openai.com/v1/responses" {
		t.Fatalf("url %q", url)
	}
}

func TestProviderRefusesUnlistedModelsAndReusesBuiltOnes(t *testing.T) {
	// Only ids in Options.Models are served, and asking again for an id returns the client already built (so its state, such as rejected reasoning items, is shared) per protocol.
	// Arrange
	p := llm.New(llm.Options{Provider: "anthropic", Models: []string{"a", "r"}, ResponsesModels: []string{"r"}})

	// Act
	_, errUnknown := p.Model(context.Background(), "nope")
	a1, errA1 := p.Model(context.Background(), "a")
	a2, errA2 := p.Model(context.Background(), "a")
	r1, errR1 := p.Model(context.Background(), "r")
	r2, errR2 := p.Model(context.Background(), "r")

	// Assert
	if errUnknown == nil || !strings.Contains(errUnknown.Error(), "nope") {
		t.Errorf("unlisted model err %v", errUnknown)
	}
	if errA1 != nil || errA2 != nil || errR1 != nil || errR2 != nil {
		t.Fatal(errA1, errA2, errR1, errR2)
	}
	if a1 != a2 || r1 != r2 || ai.Model(a1) == r1 {
		t.Errorf("clients not reused per model: %v %v %v %v", a1, a2, r1, r2)
	}
}

func TestAnthropicProviderUsesItsOwnCredentialHeaders(t *testing.T) {
	// The anthropic provider addresses /v1/messages with x-api-key and a version, not a bearer token.
	// Arrange
	var path, key, version, auth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, key, version, auth = r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"), r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`))
	}))
	t.Cleanup(ts.Close)
	m, err := llm.New(llm.Options{Provider: "anthropic", BaseURL: ts.URL, APIKey: "ak", Models: []string{"claude"}, CacheTTL: "1h"}).Model(context.Background(), "claude")
	if err != nil {
		t.Fatal(err)
	}

	// Act
	_, err = m.Generate(context.Background(), []*ai.Message{ai.UserMessage("hi")})

	// Assert
	if path != "/v1/messages" || key != "ak" || version == "" || auth != "" {
		t.Fatalf("path %q key %q version %q auth %q", path, key, version, auth)
	}
	if llm.Classify(err) != llm.ClassFatal {
		t.Fatalf("err %v classified %q, want fatal", err, llm.Classify(err))
	}
}

func TestRetryAfterIsRecordedForServerErrorsButNotClientErrors(t *testing.T) {
	// Overloaded gateways (5xx) hint when to come back just like 429s; other statuses never record a hint, whatever headers they carry.
	// Arrange
	cases := []struct {
		status int
		want   time.Duration
	}{
		{429, 3 * time.Second}, {503, 3 * time.Second}, {400, 0}, {401, 0},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				resp := jsonResponse(tc.status, `{"error":{"message":"x"}}`)
				resp.Header.Set("Retry-After", "3")
				return resp, nil
			})
			m, _ := llm.NewWithTransport(openaiConfig("http://gw.test", 0, "m"), base).Model(context.Background(), "m")
			ctx, info := llm.WithCallInfo(context.Background())

			// Act
			_, err := m.Generate(ctx, []*ai.Message{ai.UserMessage("hi")})

			// Assert
			if err == nil || info.RetryAfter() != tc.want {
				t.Fatalf("err %v retryAfter %v, want %v", err, info.RetryAfter(), tc.want)
			}
		})
	}
}

func TestTransportErrorsPassThroughTheObservingTransport(t *testing.T) {
	// A failed round trip reaches the caller unchanged and records nothing.
	// Arrange
	boom := errors.New("network down")
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, boom })
	m, _ := llm.NewWithTransport(openaiConfig("http://gw.test", 0, "m"), base).Model(context.Background(), "m")
	ctx, info := llm.WithCallInfo(context.Background())

	// Act
	_, err := m.Generate(ctx, []*ai.Message{ai.UserMessage("hi")})

	// Assert
	if !errors.Is(err, boom) || info.RetryAfter() != 0 || info.Refusal() != "" {
		t.Fatalf("err %v info %v %q", err, info.RetryAfter(), info.Refusal())
	}
}

func TestActivityIsReportedForEveryProviderEventButNotForKeepAlives(t *testing.T) {
	// OnActivity fires for each SSE event line of a call (also events the client does not forward as chunks) so a stall watchdog can reset, but not for comment keep-alives; and only on the call that registered it.
	// Arrange
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(": keep-alive\n\n" +
			"event: response.created\ndata: {\"type\":\"response.created\"}\n\n" +
			": another comment\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"delta\":\"x\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{}}\n\n"))
	}))
	t.Cleanup(ts.Close)
	m, _ := llm.New(responsesConfig(ts.URL, "m")).Model(context.Background(), "m")
	ctx, info := llm.WithCallInfo(context.Background())
	var mu sync.Mutex
	activity := 0
	info.OnActivity(func() { mu.Lock(); activity++; mu.Unlock() })
	otherCtx, other := llm.WithCallInfo(context.Background())
	_ = other

	// Act
	sr, err := m.Stream(ctx, []*ai.Message{ai.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = drain(t, sr)
	sr2, err2 := m.Stream(otherCtx, []*ai.Message{ai.UserMessage("hi")})
	if err2 != nil {
		t.Fatal(err2)
	}
	_, _ = drain(t, sr2)

	// Assert: event: + 3 data lines; the two comments do not count.
	mu.Lock()
	defer mu.Unlock()
	if err != nil || activity != 4 {
		t.Fatalf("err %v activity %d, want 4 (1 event line + 3 data lines)", err, activity)
	}
}

func TestRecordingWithoutACallInfoIsHarmless(t *testing.T) {
	// The Record* hooks are no-ops on a context that has no CallInfo, a nil CallInfo reads as empty, and a hint of zero does not erase a recorded one.
	// Arrange
	var nilInfo *llm.CallInfo
	ctx, info := llm.WithCallInfo(context.Background())

	// Act
	llm.RecordActivity(context.Background())
	llm.RecordRetryAfter(context.Background(), time.Second)
	llm.RecordRefusal(context.Background(), "no")
	nilInfo.OnActivity(func() { t.Error("callback of a nil CallInfo ran") })
	llm.RecordRetryAfter(ctx, 5*time.Second)
	llm.RecordRetryAfter(ctx, 0)
	llm.RecordRefusal(ctx, "")
	llm.RecordRefusal(ctx, "a")
	llm.RecordRefusal(ctx, "b")
	llm.RecordActivity(ctx) // no callback registered

	// Assert
	if nilInfo.RetryAfter() != 0 || nilInfo.Refusal() != "" {
		t.Error("nil CallInfo must read as empty")
	}
	if info.RetryAfter() != 5*time.Second || info.Refusal() != "ab" {
		t.Errorf("retryAfter %v refusal %q", info.RetryAfter(), info.Refusal())
	}
}

func TestParseRetryAfterReadsEveryHintAProviderUses(t *testing.T) {
	// retry-after-ms wins over Retry-After (seconds, fractional seconds or an HTTP date in the future), which wins over the rate-limit reset headers (largest of the two); unusable values yield no hint.
	// Arrange
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	future := now.Add(90 * time.Second).UTC().Format(http.TimeFormat)
	past := now.Add(-time.Minute).UTC().Format(http.TimeFormat)
	cases := []struct {
		name string
		h    map[string]string
		want time.Duration
	}{
		{"ms", map[string]string{"retry-after-ms": "250"}, 250 * time.Millisecond},
		{"ms wins over seconds", map[string]string{"retry-after-ms": "1500", "Retry-After": "30"}, 1500 * time.Millisecond},
		{"seconds", map[string]string{"Retry-After": "7"}, 7 * time.Second},
		{"fractional seconds", map[string]string{"Retry-After": " 1.5 "}, 1500 * time.Millisecond},
		{"http date", map[string]string{"Retry-After": future}, 90 * time.Second},
		{"past http date", map[string]string{"Retry-After": past}, 0},
		{"zero seconds", map[string]string{"Retry-After": "0"}, 0},
		{"negative seconds", map[string]string{"Retry-After": "-5"}, 0},
		{"garbage", map[string]string{"Retry-After": "soon", "retry-after-ms": "x"}, 0},
		{"zero ms falls through", map[string]string{"retry-after-ms": "0", "Retry-After": "2"}, 2 * time.Second},
		{"reset headers: largest", map[string]string{"x-ratelimit-reset-requests": "1s", "x-ratelimit-reset-tokens": "6m0s"}, 6 * time.Minute},
		{"reset headers: one bad", map[string]string{"x-ratelimit-reset-requests": "2s", "x-ratelimit-reset-tokens": "later"}, 2 * time.Second},
		{"retry-after wins over reset", map[string]string{"Retry-After": "4", "x-ratelimit-reset-tokens": "1m"}, 4 * time.Second},
		{"none", map[string]string{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tc.h {
				h.Set(k, v)
			}

			// Act
			got := llm.ParseRetryAfter(h, now)

			// Assert
			if got != tc.want {
				t.Fatalf("ParseRetryAfter = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRefusalIsFoundInEventsSplitAcrossReadsAndIgnoresOtherChoices(t *testing.T) {
	// Refusal text is recorded even when the body is delivered in tiny pieces and the last line (malformed, no newline) is still scanned without losing what came before; refusals of choices other than the first and null refusals are ignored.
	// Arrange
	body := "data: {\"choices\":[{\"index\":1,\"delta\":{\"refusal\":\"second choice\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"refusal\":null}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"refusal\":\"No, \"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"refusal\":\"thanks.\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"refusal\": \"broken"
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp := jsonResponse(200, "")
		resp.Header.Set("Content-Type", "text/event-stream; charset=utf-8")
		resp.Body = io.NopCloser(&oneByteReader{s: body})
		return resp, nil
	})
	m, _ := llm.NewWithTransport(openaiConfig("http://gw.test", 0, "m"), base).Model(context.Background(), "m")
	ctx, info := llm.WithCallInfo(context.Background())

	// Act
	sr, err := m.Stream(ctx, []*ai.Message{ai.UserMessage("hi")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = drain(t, sr)

	// Assert
	if info.Refusal() != "No, thanks." {
		t.Fatalf("refusal %q", info.Refusal())
	}
}

// oneByteReader delivers its text one byte per Read.
type oneByteReader struct {
	s string
	i int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	p[0] = r.s[r.i]
	r.i++
	return 1, nil
}

func TestBackoffEdgeCases(t *testing.T) {
	// Out-of-range inputs behave sensibly: a missing base defaults to one second, attempts below 1 count as the first, jitter is clamped to its range, and the exponential part never exceeds 30 s.
	// Arrange / Act
	defaulted := llm.Backoff(1, 0, 0, 0.5)
	firstAttempt := llm.Backoff(-3, time.Second, 0, 0.5)
	clampedLow := llm.Backoff(1, time.Second, 0, -9)
	clampedHigh := llm.Backoff(1, time.Second, 0, 9)
	capped := llm.Backoff(40, time.Second, 0, 0.5)
	hintBelowBackoff := llm.Backoff(3, time.Second, time.Second, 0.5)

	// Assert
	if defaulted != time.Second || firstAttempt != time.Second {
		t.Errorf("defaulted %v first %v, want 1s", defaulted, firstAttempt)
	}
	if clampedLow != 500*time.Millisecond || clampedHigh != 1500*time.Millisecond {
		t.Errorf("clamped low %v high %v", clampedLow, clampedHigh)
	}
	if capped != 30*time.Second {
		t.Errorf("capped %v, want 30s", capped)
	}
	if hintBelowBackoff != 4*time.Second {
		t.Errorf("a smaller Retry-After must not shorten the backoff: %v", hintBelowBackoff)
	}
}

func TestClassifyContextAndNetworkEdges(t *testing.T) {
	// A cancelled call is never retried, an expired deadline is a timeout even when wrapped, temporary DNS failures are transient, and a code-only overflow without a status is still an overflow.
	// Arrange
	cases := []struct {
		name string
		err  error
		want llm.ErrorClass
	}{
		{"cancelled", context.Canceled, llm.ClassFatal},
		{"wrapped cancelled", fmt.Errorf("call: %w", context.Canceled), llm.ClassFatal},
		{"deadline", fmt.Errorf("call: %w", context.DeadlineExceeded), llm.ClassTimeout},
		{"dns timeout", &net.DNSError{Err: "i/o timeout", IsTimeout: true}, llm.ClassTransient},
		{"dns temporary", &net.DNSError{Err: "try again", IsTemporary: true}, llm.ClassTransient},
		{"broken pipe", &net.OpError{Op: "write", Err: syscall.EPIPE}, llm.ClassTransient},
		{"http2 goaway text", errors.New("http2: server sent GOAWAY and closed the connection"), llm.ClassTransient},
		{"coded overflow without status", codedError{"context_length_exceeded"}, llm.ClassOverflow},
		{"other 4xx", &statusError{status: 404}, llm.ClassFatal},
		{"413 not overflow text", &statusError{status: 413, msg: "payload"}, llm.ClassFatal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			got := llm.Classify(tc.err)

			// Assert
			if got != tc.want {
				t.Fatalf("Classify(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
	if llm.Classify(nil) != "" || llm.ClassFatal.Retryable() || llm.ClassOverflow.Retryable() {
		t.Error("nil is no class; fatal and overflow are not retryable")
	}
}

type codedError struct{ code string }

func (e codedError) Error() string     { return "provider refused" }
func (e codedError) ErrorCode() string { return e.code }

type statusError struct {
	status int
	msg    string
}

func (e *statusError) Error() string   { return "failed: " + e.msg }
func (e *statusError) StatusCode() int { return e.status }

func TestDiscoverReportsUnusableAnswers(t *testing.T) {
	// A non-JSON answer, a refused connection and a cancelled context are errors that never leak the API key; the Anthropic default endpoint is used when no BaseURL is set.
	// Arrange
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>not json</html>")) }))
	t.Cleanup(bad.Close)
	gone := httptest.NewServer(http.NotFoundHandler())
	goneURL := gone.URL
	gone.Close()
	var url string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		url = r.URL.String()
		return jsonResponse(200, `{"data":[]}`), nil
	})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	// Act
	_, errBad := llm.Discover(context.Background(), bad.Client(), llm.Options{BaseURL: bad.URL, APIKey: "sk-secret"})
	_, errGone := llm.Discover(context.Background(), http.DefaultClient, llm.Options{BaseURL: goneURL, APIKey: "sk-secret"})
	_, errCancelled := llm.Discover(cancelled, bad.Client(), llm.Options{BaseURL: bad.URL})
	ids, errDefault := llm.Discover(context.Background(), &http.Client{Transport: rt}, llm.Options{Provider: "anthropic"})
	defaultAnthropic := url
	_, _ = llm.Discover(context.Background(), &http.Client{Transport: rt}, llm.Options{})
	defaultOpenAI := url

	// Assert
	if errBad == nil || errGone == nil || errCancelled == nil {
		t.Fatal("want errors", errBad, errGone, errCancelled)
	}
	for _, err := range []error{errBad, errGone} {
		if strings.Contains(err.Error(), "sk-secret") {
			t.Errorf("error leaks the key: %v", err)
		}
	}
	if errDefault != nil || len(ids) != 0 || defaultAnthropic != "https://api.anthropic.com/v1/models?limit=100" || defaultOpenAI != "https://api.openai.com/v1/models" {
		t.Errorf("ids %v err %v openai default %q", ids, errDefault, defaultOpenAI)
	}
}
