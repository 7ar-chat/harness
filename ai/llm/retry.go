package llm

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"regexp"
	"syscall"
	"time"
)

// ErrorClass sorts a failed model call by what a caller should do about it.
type ErrorClass string

// Error classes: only transient, rate-limited and timed-out calls are worth retrying as they are;
// an overflow needs a smaller request; a fatal error will not go away.
const (
	ClassFatal       ErrorClass = "fatal"
	ClassTransient   ErrorClass = "transient"
	ClassRateLimited ErrorClass = "rate_limited"
	ClassTimeout     ErrorClass = "timeout"
	ClassOverflow    ErrorClass = "overflow"
)

// Retryable reports whether a call of this class is worth re-issuing.
func (c ErrorClass) Retryable() bool {
	return c == ClassTransient || c == ClassRateLimited || c == ClassTimeout
}

// MaxRetryAfter caps a provider's Retry-After hint.
const MaxRetryAfter = 120 * time.Second

// maxBackoff caps the exponential part of a retry delay.
const maxBackoff = 30 * time.Second

var (
	overflowRe = regexp.MustCompile(`(?i)context_length_exceeded|maximum context length|prompt is too long|too many tokens|exceeds the context window|input is too long|request too large`)
	// rateRe marks throughput limits (TPM/RPM), which also say "request too large" or "too many tokens".
	rateRe      = regexp.MustCompile(`(?i)per min|rate.?limit|\bTPM\b|\bRPM\b|status code: 429`)
	transientRe = regexp.MustCompile(`(?i)status code: 5\d\d|connection reset|connection refused|broken pipe|unexpected EOF|server sent GOAWAY|stream error|INTERNAL_ERROR|http2: client connection lost`)
)

// statusOf extracts the HTTP status of a provider error (0: none).
func statusOf(err error) int {
	var status interface{ StatusCode() int }
	if errors.As(err, &status) {
		return status.StatusCode()
	}
	return 0
}

// overflowCode reports a provider error code that says the context window was exceeded.
func overflowCode(err error) bool {
	var coded interface{ ErrorCode() string }
	return errors.As(err, &coded) && overflowRe.MatchString(coded.ErrorCode())
}

// Classify sorts a model error. A 429 is always a rate limit (even "Request too
// large … tokens per min"); a context overflow needs a 400/413 status, the
// context_length_exceeded code or, without a status, an overflow message that
// is not about throughput; network errors retry only on timeouts, resets and
// refused connections (DNS and TLS failures are fatal).
func Classify(err error) ErrorClass {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return ClassFatal
	case errors.Is(err, context.DeadlineExceeded):
		return ClassTimeout
	}
	msg := err.Error()
	switch code := statusOf(err); {
	case code == http.StatusTooManyRequests:
		return ClassRateLimited
	case code == http.StatusBadRequest || code == http.StatusRequestEntityTooLarge:
		if overflowCode(err) || overflowRe.MatchString(msg) {
			return ClassOverflow
		}
		return ClassFatal
	case code == http.StatusRequestTimeout || code >= 500:
		return ClassTransient
	case code > 0:
		return ClassFatal
	}
	if overflowCode(err) {
		return ClassOverflow
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsTimeout || dnsErr.IsTemporary {
			return ClassTransient
		}
		return ClassFatal
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ClassTimeout
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
		return ClassTransient
	}
	switch {
	case rateRe.MatchString(msg):
		return ClassRateLimited
	case overflowRe.MatchString(msg):
		return ClassOverflow
	case transientRe.MatchString(msg):
		return ClassTransient
	}
	return ClassFatal
}

// IsTransient reports whether a model error is worth retrying (timeouts, 429, 5xx, resets).
func IsTransient(err error) bool { return Classify(err).Retryable() }

// IsContextOverflow reports whether a model error says the request does not fit
// the model's context window. It is never transient: retrying the same request cannot help, the caller has to
// shrink it first (see package compaction).
func IsContextOverflow(err error) bool { return Classify(err) == ClassOverflow }

// Backoff is the delay before retry n (1 = first retry): base·2^(n-1), capped
// at 30 s, scaled by a jitter factor in [0.5, 1.5) (jitter ∈ [0, 1)), and never
// below the provider's Retry-After hint (capped at MaxRetryAfter).
func Backoff(n int, base, retryAfter time.Duration, jitter float64) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	exp := float64(base) * math.Pow(2, float64(max(n, 1)-1))
	exp = math.Min(exp, float64(maxBackoff))
	d := time.Duration(exp * (0.5 + math.Max(0, math.Min(jitter, 1))))
	if retryAfter > 0 {
		d = max(d, min(retryAfter, MaxRetryAfter))
	}
	return d
}
