# Infrastructure and sessions

## Infrastructure

The engine, sandbox and feature packages do not know Redis (or any broker). They define the small interfaces
they need, with in-process implementations, and `harness.Infra` bundles the implementations a deployment
injects (`Options.Infra`; every field optional, a missing one stays in process):

| Field | Interface | In process | Redis (`redisx`) |
|---|---|---|---|
| `Broker` | `queue.Queue` + `queue.Bus` + `runtime.RunQueue`: run queue, cancel, steers | `queue.NewMemory` | `redisx.NewQueue` |
| `Log` | `eventlog.Log`: per-run event log with replay | `eventlog.NewMemory` | `redisx.NewEventLog` |
| `Coordinator` | `sandbox.Coordinator` + `runtime.Locks`: creation locks, health streaks, sweeper and job locks, run liveness | none (`sandbox.LocalCoordinator` inside the manager) | `redisx.NewCoordinator` |
| `Jobs` | `sandboxtools.JobList`: running background jobs and their leases | memory only | `redisx.NewJobs` |
| `Installs` | `skills.InstallState`: installed skill revisions and install locks | per process | `redisx.NewSkillInstalls` |

Package `redisinfra` builds all five over a Redis client:

```go
rdb := redisinfra.NewClient(cfg)              // REDIS_*, USE_SENTINEL
infra := redisinfra.Infra(rdb, store)
h, err := harness.New(harness.Options{Config: cfg, Store: store, Infra: &infra})
```

It is its own package so that a program that does not use Redis does not link go-redis. To run on NATS or SQS,
implement the interfaces above and pass them in `Infra`; nothing else changes.

## Sessions

Local hosts keep their conversations across restarts without Postgres or Redis:

```go
st, _ := harness.OpenLocalStore("")           // "" = $LOCAL_STATE_DIR, else ~/.har/sessions ($HAR_HOME/sessions)
defer st.Close()
h, _ := harness.New(harness.Options{Config: cfg, Store: st})

ss := st.Sessions()
list, _ := ss.List(ctx, "alice", 20)          // title, updated, last message preview, active run
fork, _ := ss.Fork(ctx, "alice", threadID, messageID) // new thread with the history up to that message
ss.Delete(ctx, "alice", threadID)
// resume = keep using the thread id: h.Send(ctx, "alice", threadID, "…")
// old runs replay: h.Events(ctx, oldRunID, 0)
```

`adapters/store/filestore` wraps the in-memory store and appends every change to
`<dir>/journal.jsonl` (JSON lines, upsert records, one `write(2)` per batch). Threads, messages,
runs, plans, summaries, compactions, goals, approvals and event logs persist; artifacts, blobs
and sandbox ids stay in memory. A `kill -9` loses nothing and can only tear the last
line, which is cut at the next start; fsync happens every 500 ms and on `Close` (`SyncAlways` /
`SyncNever` via `filestore.Open(dir, filestore.Options{…})`). Runs that were in flight when the
process died are marked `interrupted` (with a terminal `RUN_ERROR`) at the next start. One process
per directory (flock). The journal is compacted on open when it holds many superseded records or
deleted threads.

## Sandbox definition

`sandbox-image/` holds the sandbox image (`Dockerfile`: Python/Node/LibreOffice/pandoc/… toolchain,
build args for mirrors, optional CA certificates in `certs/`) and the OpenSandbox server config
(`opensandbox.toml`) with the isolation model (loopback-only ports, deny-by-default egress policy).
The Go side is `sandbox/` (`OpenSandbox` backend, `Manager`: one sandbox per thread, quota,
health, recreation notice, keep-alive, `Coordinator` for cross-replica locks).

