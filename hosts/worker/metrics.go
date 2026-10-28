package worker

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GhaziBenDahmane/harness/engine/runtime"
	"github.com/GhaziBenDahmane/harness/store"
)

// metrics counts what the worker can see without instrumenting the engine: runs starting (an agent
// run-start hook) and ending (an end-of-run runtime hook). It implements both hooks.
type metrics struct {
	start time.Time

	mu       sync.Mutex
	inflight map[string]struct{}
	started  int64
	finished map[string]int64 // by terminal status

	draining atomic.Bool
}

func newMetrics() *metrics {
	return &metrics{start: time.Now(), inflight: map[string]struct{}{}, finished: map[string]int64{}}
}

// BeforeRun implements agent.RunStartHook: a run begins its work.
func (m *metrics) BeforeRun(_ context.Context, run *store.Run) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.inflight[run.ID]; !ok {
		m.inflight[run.ID] = struct{}{}
		m.started++
	}
}

// Finishing implements runtime.Hook: a run ends with f.Status.
func (m *metrics) Finishing(_ context.Context, f *runtime.Finish) func(context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inflight, f.Run.ID)
	m.finished[f.Status]++
	return nil
}

// InFlight is the number of runs between their start and their end on this worker.
func (m *metrics) InFlight() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.inflight)
}

// Write renders the Prometheus text exposition format.
func (m *metrics) Write(w io.Writer, ready bool, concurrency int) {
	m.mu.Lock()
	inflight, started := len(m.inflight), m.started
	finished := make(map[string]int64, len(m.finished))
	for k, v := range m.finished {
		finished[k] = v
	}
	m.mu.Unlock()
	b2i := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	fmt.Fprintf(w, "# HELP harness_worker_up The worker process is running.\n# TYPE harness_worker_up gauge\nharness_worker_up 1\n")
	fmt.Fprintf(w, "# HELP harness_worker_ready The worker consumes the run queue (readyz).\n# TYPE harness_worker_ready gauge\nharness_worker_ready %d\n", b2i(ready))
	fmt.Fprintf(w, "# HELP harness_worker_draining The worker is shutting down and no longer claims runs.\n# TYPE harness_worker_draining gauge\nharness_worker_draining %d\n", b2i(m.draining.Load()))
	fmt.Fprintf(w, "# HELP harness_worker_uptime_seconds Seconds since the worker started.\n# TYPE harness_worker_uptime_seconds gauge\nharness_worker_uptime_seconds %.0f\n", time.Since(m.start).Seconds())
	fmt.Fprintf(w, "# HELP harness_worker_concurrency Configured maximum of concurrent runs.\n# TYPE harness_worker_concurrency gauge\nharness_worker_concurrency %d\n", concurrency)
	fmt.Fprintf(w, "# HELP harness_worker_runs_in_flight Runs executing on this worker.\n# TYPE harness_worker_runs_in_flight gauge\nharness_worker_runs_in_flight %d\n", inflight)
	fmt.Fprintf(w, "# HELP harness_worker_runs_started_total Runs this worker started (compaction runs excluded).\n# TYPE harness_worker_runs_started_total counter\nharness_worker_runs_started_total %d\n", started)
	fmt.Fprintf(w, "# HELP harness_worker_runs_finished_total Runs this worker finished, by status.\n# TYPE harness_worker_runs_finished_total counter\n")
	statuses := make([]string, 0, len(finished))
	for s := range finished {
		statuses = append(statuses, s)
	}
	sort.Strings(statuses)
	for _, s := range statuses {
		fmt.Fprintf(w, "harness_worker_runs_finished_total{status=%q} %d\n", s, finished[s])
	}
}
