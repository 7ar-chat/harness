package llm_test

import (
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"syscall"
	"testing"
	"time"

	goopenai "github.com/meguminnnnnnnnn/go-openai"

	"github.com/GhaziBenDahmane/harness/llm"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestModelErrorsAreClassifiedForRetries(t *testing.T) {
	// Only timeouts, 429s, 5xx and connection resets are retried; a 429 "tokens per min" is a rate limit, never a context overflow; DNS/x509/4xx are fatal.
	// Arrange
	cases := []struct {
		name string
		err  error
		want llm.ErrorClass
	}{
		{"429 TPM", &goopenai.APIError{HTTPStatusCode: 429, Message: "Request too large for gpt-5 on tokens per min (TPM): Limit 30000, Requested 50000"}, llm.ClassRateLimited},
		{"429 too many tokens", &goopenai.RequestError{HTTPStatusCode: 429, Err: errors.New("too many tokens per minute")}, llm.ClassRateLimited},
		{"429 text", errors.New("error, status code: 429, message: Request too large for gpt-5 on tokens per min"), llm.ClassRateLimited},
		{"500 TPM text", &goopenai.APIError{HTTPStatusCode: 500, Message: "request too large for gpt-5 on tokens per min"}, llm.ClassTransient},
		{"503", &goopenai.RequestError{HTTPStatusCode: 503, Err: errors.New("unavailable")}, llm.ClassTransient},
		{"408", &goopenai.APIError{HTTPStatusCode: 408, Message: "timeout"}, llm.ClassTransient},
		{"400 overflow", &goopenai.APIError{HTTPStatusCode: 400, Code: "context_length_exceeded", Message: "maximum context length is 128000 tokens"}, llm.ClassOverflow},
		{"413 overflow", &goopenai.APIError{HTTPStatusCode: 413, Message: "Request too large"}, llm.ClassOverflow},
		{"400 bad", &goopenai.APIError{HTTPStatusCode: 400, Message: "bad request"}, llm.ClassFatal},
		{"401", &goopenai.APIError{HTTPStatusCode: 401, Message: "unauthorized"}, llm.ClassFatal},
		{"conn reset", &url.Error{Op: "Post", URL: "http://gw", Err: &net.OpError{Op: "read", Err: syscall.ECONNRESET}}, llm.ClassTransient},
		{"conn refused", &url.Error{Op: "Post", URL: "http://gw", Err: &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}}, llm.ClassTransient},
		{"net timeout", &url.Error{Op: "Post", URL: "http://gw", Err: timeoutErr{}}, llm.ClassTimeout},
		{"unexpected EOF", fmt.Errorf("failed to receive stream chunk: %w", io.ErrUnexpectedEOF), llm.ClassTransient},
		{"dns", &url.Error{Op: "Post", URL: "http://gw", Err: &net.DNSError{Err: "no such host", Name: "gw", IsNotFound: true}}, llm.ClassFatal},
		{"x509", &url.Error{Op: "Post", URL: "https://gw", Err: x509.UnknownAuthorityError{}}, llm.ClassFatal},
		{"plain", errors.New("invalid api key"), llm.ClassFatal},
	}

	// Act
	got := map[string]llm.ErrorClass{}
	for _, c := range cases {
		got[c.name] = llm.Classify(c.err)
	}

	// Assert
	for _, c := range cases {
		if got[c.name] != c.want {
			t.Errorf("%s: got %q want %q", c.name, got[c.name], c.want)
		}
		if retry := llm.IsTransient(c.err); retry != (c.want == llm.ClassTransient || c.want == llm.ClassRateLimited || c.want == llm.ClassTimeout) {
			t.Errorf("%s: IsTransient %v", c.name, retry)
		}
		if llm.IsContextOverflow(c.err) != (c.want == llm.ClassOverflow) {
			t.Errorf("%s: IsContextOverflow %v", c.name, !(c.want == llm.ClassOverflow))
		}
	}
}

func TestBackoffGrowsWithJitterAndHonoursRetryAfter(t *testing.T) {
	// Retry delays double per attempt with ±50 % jitter, and a provider Retry-After (capped at 120 s) is never undercut.
	// Arrange
	base := time.Second

	// Act
	low, high := llm.Backoff(3, base, 0, 0), llm.Backoff(3, base, 0, 0.999)
	hinted := llm.Backoff(1, base, 30*time.Second, 0)
	capped := llm.Backoff(1, base, time.Hour, 0)

	// Assert
	if low != 2*time.Second || high < 5900*time.Millisecond || high > 6*time.Second {
		t.Errorf("attempt 3: low %v high %v (want 2s..6s)", low, high)
	}
	if hinted != 30*time.Second || capped != 120*time.Second {
		t.Errorf("retry-after: hinted %v capped %v", hinted, capped)
	}
}
