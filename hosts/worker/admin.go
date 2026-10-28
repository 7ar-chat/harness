package worker

import (
	"errors"
	"fmt"
	"net/http"
)

// admin serves /healthz, /readyz and /metrics. health is the runtime's queue-loop health.
func admin(health func() error, m *metrics, concurrency int) http.Handler {
	ready := func() error {
		if m.draining.Load() {
			return errors.New("draining: no longer claiming runs")
		}
		return health()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if err := ready(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, "not ready:", err)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		m.Write(w, ready() == nil, concurrency)
	})
	return mux
}
