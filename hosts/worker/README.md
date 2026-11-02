# hosts/worker

`app/cmd/harness-worker` (package `hosts/worker`) is a headless process that executes runs queued by other
processes: a web server or API calls `Send` (it never calls `Run`), and any number of workers consume the
shared Redis queue and Postgres store, each with its own sandboxes. Intelligence and sandboxes scale
independently of the front end.

| Setting | Meaning |
|---|---|
| `REDIS_*`, `POSTGRES_*` | the shared queue/event log and store (the same values as the sending process; migrations are applied at start) |
| `LLM_*`, `OPENSANDBOX_URL` / `SANDBOX_BACKEND` | models and sandbox backend of this worker |
| `RUNTIME_CONCURRENCY` or `-concurrency N` | maximum concurrent runs (default 16) |
| `RUNTIME_SHUTDOWN_GRACE_SECONDS` | on SIGINT/SIGTERM the worker stops claiming and lets runs finish this long; the rest end `interrupted` (never lost) |
| `HARNESS_WORKER_ADDR` or `-addr` | admin server, default `127.0.0.1:9090` |
| `-once` | process queued runs until none is queued or running, then exit 0 (batch, CI) |
| `-standalone` | allow a private in-memory queue/store; without it the worker refuses to start unless Redis and Postgres are configured |
| `-id` | worker id recorded as the run owner (default hostname-random) |

Admin endpoints: `GET /healthz` (process is up), `GET /readyz` (200 while the queue loop reads; 503 when the
consumer group cannot be created, the loop is stuck, or the worker is draining), `GET /metrics` (Prometheus
text: `harness_worker_up`, `_ready`, `_draining`, `_uptime_seconds`, `_concurrency`, `_runs_in_flight`,
`_runs_started_total`, `_runs_finished_total{status}`). The startup log names the store, queue and sandbox
backend and the model list.

A docker-compose deployment is one `redis` and one `postgres` service, a web service built on the library
(environment: the same `REDIS_HOST`/`POSTGRES_HOST`), and a `worker` service running `harness-worker` with
those variables plus `LLM_API_KEY` and `OPENSANDBOX_URL`, `HARNESS_WORKER_ADDR=0.0.0.0:9090`, a health check
on `/readyz`, `stop_grace_period` longer than `RUNTIME_SHUTDOWN_GRACE_SECONDS`, and `deploy.replicas` set to
the number of workers wanted.

