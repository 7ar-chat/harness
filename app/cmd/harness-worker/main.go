// Command harness-worker is the headless worker of the harness: it executes agent runs queued by
// other processes (a web server, an API, a scheduler) through the shared Redis queue and Postgres store.
//
//	REDIS_HOST=redis POSTGRES_HOST=db LLM_API_KEY=… OPENSANDBOX_URL=http://sandboxes:8090 harness-worker
//
// Admin endpoints on HARNESS_WORKER_ADDR (default 127.0.0.1:9090): /healthz, /readyz, /metrics.
// SIGINT/SIGTERM stop claiming runs and drain in-flight ones for RUNTIME_SHUTDOWN_GRACE_SECONDS.
//
// # Agent factory
//
// With FACTORY=1 the worker installs the agent factory in its harness, so the runs of factory tasks it executes
// get their end-of-run hooks (the task moves on, its spend is metered), the open_pr and delegate tools, the
// autonomy policy and the budget guard. Those act in the process that executes a run: every worker must run with
// the same FACTORY_* settings as harness-web (docs/factory.md, "Deployment shape"). The tasks live in the worker's
// Postgres (POSTGRES_*), the GitHub App comes from GITHUB_APP_* as in harness-web.
//
// A worker executes runs; it does not dispatch tasks. The dispatcher, the cron scheduler and the pull request loop
// stay off unless FACTORY_RUN=1 (the default of harness-web is the opposite), so they run in the web tier or in the
// one worker that opts in. The product-only pieces stay in the web tier too: task threads are not linked to a
// project, and a task that needs a human is only logged here (the live feed and Telegram belong to harness-web).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/hosts/worker"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stderr)
	stop() // a second signal during the drain kills the process
	os.Exit(code)
}

func run(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("harness-worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o worker.Options
	var concurrency int
	fs.IntVar(&concurrency, "concurrency", 0, "maximum concurrent runs (default RUNTIME_CONCURRENCY)")
	fs.StringVar(&o.Addr, "addr", "", "admin listen address (default HARNESS_WORKER_ADDR, else "+worker.DefaultAddr+")")
	fs.BoolVar(&o.Once, "once", false, "process the queued runs until the queue is empty, then exit")
	fs.BoolVar(&o.Standalone, "standalone", false, "allow a private in-memory queue and store (no other process can send work)")
	fs.StringVar(&o.RuntimeID, "id", "", "worker id in run ownership (default hostname-random)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if concurrency > 0 {
		_ = os.Setenv("RUNTIME_CONCURRENCY", strconv.Itoa(concurrency))
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, "harness-worker:", err)
		return 1
	}
	o.Config = cfg
	o.Features = factoryFeatures(os.Getenv, o.Standalone)
	if err := worker.Run(ctx, o); err != nil {
		fmt.Fprintln(stderr, "harness-worker:", err)
		return 1
	}
	return 0
}
