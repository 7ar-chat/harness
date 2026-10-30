package errtrack_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/adapters/errtrack"
)

// tracker is a stand-in Sentry server: it records the events posted to /api/1/envelope/.
type tracker struct {
	srv    *httptest.Server
	mu     sync.Mutex
	events []map[string]any
	auth   []string
}

func newTracker(t *testing.T) *tracker {
	t.Helper()
	tr := &tracker{}
	tr.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Errorf("gzip: %v", err)
				return
			}
			body = zr
		}
		raw, _ := io.ReadAll(body)
		tr.mu.Lock()
		defer tr.mu.Unlock()
		tr.auth = append(tr.auth, r.Header.Get("X-Sentry-Auth"))
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") { // envelope: header, item header, payload
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) == nil && m["exception"] != nil {
				tr.events = append(tr.events, m)
			}
		}
	}))
	t.Cleanup(tr.srv.Close)
	return tr
}

func (tr *tracker) dsn() string {
	return strings.Replace(tr.srv.URL, "http://", "http://publickey@", 1) + "/1"
}

func (tr *tracker) got() []map[string]any {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]map[string]any(nil), tr.events...)
}

func logger(t *testing.T, tr *tracker) (*slog.Logger, *errtrack.Tracker) {
	t.Helper()
	tk, err := errtrack.New(errtrack.Options{DSN: tr.dsn(), Environment: "production", Release: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	inner := slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn})
	return slog.New(tk.Handler(inner)), tk
}

func exception(ev map[string]any) map[string]any {
	return ev["exception"].([]any)[0].(map[string]any)
}

func TestAnErrorRecordIsReportedWithItsErrorAndAttributes(t *testing.T) {
	// Arrange
	tr := newTracker(t)
	log, tk := logger(t, tr)

	// Act
	log.Error("run failed", "run_id", "r-42", "thread_id", "t-7", "error", errors.New("model unreachable"), "steps", 3)
	tk.Flush(5 * time.Second)

	// Assert
	evs := tr.got()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	ev := evs[0]
	exc := exception(ev)
	tags := ev["tags"].(map[string]any)
	logCtx := ev["contexts"].(map[string]any)["log"].(map[string]any)
	if exc["value"] != "model unreachable" || ev["message"] != "run failed" ||
		tags["run_id"] != "r-42" || tags["thread_id"] != "t-7" ||
		logCtx["steps"] != float64(3) || ev["environment"] != "production" || ev["release"] != "r1" {
		t.Errorf("event = %v", ev)
	}
	if fp := ev["fingerprint"].([]any); len(fp) != 1 || fp[0] != "run failed" {
		t.Errorf("fingerprint = %v, want the log message alone so every failed run is one issue", fp)
	}
}

func TestTheStackPointsAtTheCallerNotAtTheLogger(t *testing.T) {
	// Arrange
	tr := newTracker(t)
	log, tk := logger(t, tr)

	// Act
	func() { log.Error("storage failed", "error", errors.New("disk full")) }()
	tk.Flush(5 * time.Second)

	// Assert
	frames := exception(tr.got()[0])["stacktrace"].(map[string]any)["frames"].([]any)
	last := frames[len(frames)-1].(map[string]any)
	if !strings.HasSuffix(fmt.Sprint(last["abs_path"]), "errtrack_test.go") {
		t.Errorf("innermost frame = %v, want the test file that logged (not log/slog or errtrack)", last)
	}
}

func TestAPanicIsReportedAsAPanic(t *testing.T) {
	// Arrange
	tr := newTracker(t)
	log, tk := logger(t, tr)

	// Act
	log.Error("agent panic", "run_id", "r-1", "panic", "index out of range")
	tk.Flush(5 * time.Second)

	// Assert
	exc := exception(tr.got()[0])
	if exc["type"] != "panic" || exc["value"] != "index out of range" {
		t.Errorf("exception = %v", exc)
	}
}

func TestWarningsAndCancellationsAreNotReported(t *testing.T) {
	// Arrange
	tr := newTracker(t)
	log, tk := logger(t, tr)

	// Act
	log.Warn("slow tool", "ms", 900)
	log.Info("started")
	log.Error("client: request failed", "error", context.Canceled)
	log.Error("wrapped", "error", fmt.Errorf("while reading: %w", context.Canceled))
	tk.Flush(2 * time.Second)

	// Assert
	if n := len(tr.got()); n != 0 {
		t.Errorf("events = %d, want none", n)
	}
}

func TestAttributesFromWithAttrsAndGroupsAreKept(t *testing.T) {
	// Arrange
	tr := newTracker(t)
	log, tk := logger(t, tr)

	// Act
	log.With("run_id", "r-9").WithGroup("req").Error("httpserver: request failed", "error", errors.New("boom"), "path", "/api/x")
	tk.Flush(5 * time.Second)

	// Assert
	ev := tr.got()[0]
	logCtx := ev["contexts"].(map[string]any)["log"].(map[string]any)
	if ev["tags"].(map[string]any)["run_id"] != "r-9" || logCtx["req.path"] != "/api/x" {
		t.Errorf("event = %v", ev)
	}
}

func TestTheSamePanicFromDifferentClientsIsOneIssue(t *testing.T) {
	// Arrange
	tr := newTracker(t)
	log, tk := logger(t, tr)

	// Act: net/http's message embeds the client address and a goroutine dump
	log.Error("http: panic serving 10.0.0.7:51234: boom\ngoroutine 33 [running]:\nmain.handler()")
	log.Error("http: panic serving 10.0.0.9:60001: boom\ngoroutine 98 [running]:\nmain.other()")
	log.Error("http: panic serving 10.0.0.9:60001: a different failure")
	tk.Flush(5 * time.Second)

	// Assert
	evs := tr.got()
	if len(evs) != 3 {
		t.Fatalf("events = %d, want 3", len(evs))
	}
	fp := func(i int) string { return evs[i]["fingerprint"].([]any)[0].(string) }
	if fp(0) != fp(1) {
		t.Errorf("same panic, different client: %q vs %q", fp(0), fp(1))
	}
	if fp(0) == fp(2) {
		t.Errorf("different failures share the fingerprint %q", fp(0))
	}
}

func TestTheInnerHandlerStillSeesWhatItWouldHave(t *testing.T) {
	// Arrange
	tr := newTracker(t)
	var out strings.Builder
	tk, _ := errtrack.New(errtrack.Options{DSN: tr.dsn()})
	log := slog.New(tk.Handler(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelWarn})))

	// Act
	log.Info("quiet")
	log.Warn("loud")
	log.Error("fatal", "error", errors.New("x"))
	tk.Flush(5 * time.Second)

	// Assert
	if s := out.String(); strings.Contains(s, "quiet") || !strings.Contains(s, "loud") || !strings.Contains(s, "fatal") {
		t.Errorf("inner output = %q", s)
	}
}

func TestAnUnreachableTrackerNeverBlocksTheCaller(t *testing.T) {
	// Arrange
	tk, err := errtrack.New(errtrack.Options{DSN: "http://k@127.0.0.1:1/1"})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(tk.Handler(slog.NewTextHandler(io.Discard, nil)))

	// Act
	start := time.Now()
	for range 50 {
		log.Error("run failed", "error", errors.New("x"))
	}

	// Assert
	if d := time.Since(start); d > time.Second {
		t.Errorf("50 reports took %v; sending must happen in the background", d)
	}
}

func TestNewRejectsAnEmptyOrMalformedDSN(t *testing.T) {
	for _, dsn := range []string{"", "not a dsn"} {
		if _, err := errtrack.New(errtrack.Options{DSN: dsn}); err == nil {
			t.Errorf("DSN %q accepted", dsn)
		}
	}
}
