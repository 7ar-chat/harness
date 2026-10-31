package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/runtime"
)

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func TestReadyzFollowsTheQueueLoopHealthAndTheDrain(t *testing.T) {
	// Arrange
	m := newMetrics()
	var health error
	h := admin(func() error { return health }, m, 4)

	// Act / Assert: healthy
	if code, _ := get(t, h, "/readyz"); code != http.StatusOK {
		t.Fatalf("healthy readyz = %d", code)
	}
	// the queue loop is stuck
	health = errors.New("queue loop stuck")
	if code, body := get(t, h, "/readyz"); code != http.StatusServiceUnavailable || !strings.Contains(body, "stuck") {
		t.Fatalf("stuck readyz = %d %q", code, body)
	}
	// draining, even though the loop is fine: stop receiving traffic, but the process is alive
	health = nil
	m.draining.Store(true)
	if code, body := get(t, h, "/readyz"); code != http.StatusServiceUnavailable || !strings.Contains(body, "draining") {
		t.Fatalf("draining readyz = %d %q", code, body)
	}
	if code, _ := get(t, h, "/healthz"); code != http.StatusOK {
		t.Fatalf("healthz while draining = %d", code)
	}
}

func TestMetricsCountRunsInFlightAndFinishedByStatus(t *testing.T) {
	// Arrange
	m := newMetrics()
	ctx := context.Background()
	a, b := &store.Run{ID: "a"}, &store.Run{ID: "b"}

	// Act
	m.BeforeRun(ctx, a)
	m.BeforeRun(ctx, a) // a repeated start does not count twice
	m.BeforeRun(ctx, b)
	m.Finishing(ctx, &runtime.Finish{Run: a, Status: store.StatusCompleted})
	inFlight := m.InFlight()
	m.Finishing(ctx, &runtime.Finish{Run: b, Status: store.StatusInterrupted})
	var out bytes.Buffer
	m.Write(&out, true, 8)

	// Assert
	if inFlight != 1 || m.InFlight() != 0 {
		t.Fatalf("in flight %d then %d", inFlight, m.InFlight())
	}
	for _, want := range []string{
		"harness_worker_up 1", "harness_worker_ready 1", "harness_worker_draining 0", "harness_worker_concurrency 8",
		"harness_worker_runs_in_flight 0", "harness_worker_runs_started_total 2",
		`harness_worker_runs_finished_total{status="completed"} 1`,
		`harness_worker_runs_finished_total{status="interrupted"} 1`,
		"# TYPE harness_worker_uptime_seconds gauge",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, out.String())
		}
	}
}

func TestRunWithoutASharedStoreRefusesToStart(t *testing.T) {
	// Arrange: no Store supplied, no POSTGRES_HOST, not standalone.
	t.Setenv("POSTGRES_HOST", "")
	_ = os.Unsetenv("POSTGRES_HOST")
	t.Setenv("PING_DB_AT_STARTUP", "false")
	t.Setenv("LLM_MODELS", "gpt-test")

	// Act
	err := Run(context.Background(), Options{Addr: "127.0.0.1:0"})

	// Assert
	if err == nil || !strings.Contains(err.Error(), "POSTGRES_HOST") {
		t.Fatalf("err = %v", err)
	}
}

func TestStandaloneWorkerServesTheAdminEndpointsAndStopsGracefullyOnCancel(t *testing.T) {
	// Arrange
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("LLM_MODELS", "gpt-test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := make(chan string, 1)
	done := make(chan error, 1)

	// Act
	go func() {
		done <- Run(ctx, Options{
			Addr: "127.0.0.1:0", Standalone: true, Backend: sandboxtest.NewBackend(),
			Models: llmtest.Factory{M: llmtest.New()}, OnListening: func(a string) { addr <- a },
		})
	}()
	base := "http://" + <-addr
	waitFor(t, "readyz 200", func() bool { return status(base+"/readyz") == http.StatusOK })

	// Assert
	if got := status(base + "/healthz"); got != http.StatusOK {
		t.Fatalf("healthz = %d", got)
	}
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "harness_worker_ready 1") {
		t.Fatalf("metrics:\n%s", body)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not stop")
	}
}

func status(url string) int {
	resp, err := http.Get(url)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
