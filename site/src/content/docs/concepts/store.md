---
title: The store
description: "What the harness persists and where (threads, runs, messages, events and more), the store interfaces, and the three backends: memory by default, a local file journal, Postgres."
---

The **store** is the persistence port of the harness: threads, runs, messages and the event log of each run, plus the data of the optional capabilities (artifacts, plans, goals, approvals, memory). You pass one as `Options.Store`; with none, `harness.New` uses an in-memory store. Choose by how many processes you run and whether a restart may lose conversations:

| Backend | Package | Use it for | Survives a restart | Several processes |
|---|---|---|---|---|
| Memory (default) | `adapters/store/memstore` | tests, demos, a throwaway process | no | no |
| File journal | `adapters/store/filestore` | CLI, TUI, desktop, a single-node server | threads, messages, runs, events, approvals, goals, memory | no (one process per directory) |
| Postgres | `adapters/store/pgstore` | web plus workers, production | everything | yes |

Which backend a deployment uses is also a [deployment shape](/docs/guides/deployment-shapes/); the connection details are on [Stores](/docs/integrations/stores/).

## Store interfaces

The ports live in `core/store`. A package asks for the smallest interface it uses.

| Interface | Holds |
|---|---|
| `store.Store` | The core: threads, runs, messages, the event journal (`AppendEvents`, `ListEvents`, `LastEvent`), run claiming and heartbeats, `InterruptStaleRuns`. Everything the engine needs to run a conversation. |
| `store.Full` | `Store` plus the stores of the optional capabilities: artifacts and blobs, skills, plans/goals/feedback (`HarnessStore`), approvals, memory, notifications. Memory, file and Postgres backends implement it. |
| `harness.Store` | `store.Full` plus `sandbox.Store` (thread to sandbox mapping) and `store.WorkspaceGitStore`. This is the type of `Options.Store`. |

`store.Full` holds the types of every capability, so a feature cannot bring its own tables without a change to `core/store` ([Known gaps](https://github.com/7ar-chat/harness/blob/main/docs/architecture.md#known-gaps) in the architecture notes). A feature that needs more than `store.Store` asks for it with `harness.StoreAs[T](b)`, which fails `New` with `feature "x" needs a store implementing <type>` if the store lacks it, for instance a custom store passed as `Options.Store` that omits an extension.

## Memory: the default

`harness.New` builds `memstore.New()` when `Options.Store` is nil, and keeps the event log in the same process. Nothing needs to run, nothing is written to disk. When the process exits, every thread, run and event is gone. It is goroutine-safe and implements the whole of `store.Full`.

The package comment of `memstore` says "used by tests (and nothing else)"; in practice it is the default of `harness.New`, which is why this page calls it the zero-config mode.

## filestore: a journal on disk

`harness.OpenLocalStore(dir)` opens a durable store with no Postgres or Redis. It wraps the memory store and appends every change to a journal in the state directory, so a closed terminal or a `kill -9` never loses a session.

```go
st, err := harness.OpenLocalStore("") // "" = DefaultLocalStoreDir()
if err != nil {
	return err
}
defer st.Close() // flushes and releases the directory

h, err := harness.New(harness.Options{Config: cfg, Store: st})
```

`DefaultLocalStoreDir()` is `$LOCAL_STATE_DIR`, else `<user dir>/sessions` (`$HAR_HOME` or `~/.har`), else `$XDG_STATE_HOME/harness`, else `~/.local/state/harness`.

### How it persists

- **Journal.** Every mutating call is applied to memory first, then the entities it changed are appended to `<dir>/journal.jsonl` as idempotent JSON-lines upsert records (one `write(2)` per batch, on an `O_APPEND` file). Reads are served from memory. Unchanged records are skipped, so heartbeats cost nothing. Open replays the journal.
- **A crash loses nothing.** The bytes are in the kernel when the call returns. A crash can only tear the last line, which `Open` detects and cuts (`Store.Truncated` reports it). A bad line anywhere else is `filestore.ErrCorrupt`, not silently skipped.
- **Sync modes.** `filestore.Options{Sync: ...}` picks when the journal is fsynced.

| `Sync` | Behaviour |
|---|---|
| `SyncInterval` (default) | fsync at most every 500 ms (`SyncEvery` overrides) and on `Close`. A power failure loses at most that window. |
| `SyncAlways` | fsync after every batch. |
| `SyncNever` | Never fsync. Process crashes are still safe; power loss is not. |

- **One process per directory.** `Open` takes an exclusive advisory `flock` on `<dir>/LOCK`; a second process gets `filestore: state directory is in use by another process`. The lock dies with the process, also on `kill -9`.
- **Compaction.** On `Open` the journal is rewritten from the live state (temp file and atomic rename) when it holds many superseded records or deleted threads.
- **Crash recovery.** Every run that was queued or running when the previous process died is marked `interrupted`, and its event log gets a `RUN_ERROR` with code `interrupted` (message "The run was interrupted because the application stopped.") so replays terminate. `Store.Recovered` counts them.

<div class="callout note">

**What stays in memory only.** Per the package documentation: artifacts and blobs, the skills catalog, workspace git archives and sandbox ids are rebuilt or recreated on demand and are not journaled. Threads (title, pin, plan, pending question, interaction mode), messages, runs, event logs, approvals, goals, feedback, summaries and compactions are. The journal also holds memory entries (the `mem` and `memoff` records), though the package comment does not list them.

</div>

This program proves the recovery. Process 1 finishes one run and leaves a second one queued when it stops; process 2 opens the same directory:

```go
h, closeStore, err := newHarness(dir) // OpenLocalStore(dir) + harness.New with a scripted model
// ... send "hello", wait for RUN_FINISHED, stop h.Run, then send "are you there?" (stays queued) ...
closeStore()

h2, closeStore2, err := newHarness(dir) // the same directory, as if after a restart
```

```text
runs marked interrupted at open: 0
runs marked interrupted at open: 1
run d1abb402: status=completed code="" last event=RUN_FINISHED
run 67b063f3: status=interrupted code="interrupted" last event=RUN_ERROR
messages kept: 3
```

The completed run replays in full (`h2.Events(ctx, runID, 0)`); the one that was in flight is `interrupted` and ends with a terminal event, and the thread's three messages (two user messages and the answer) are still there.

### Sessions

`Store.Sessions()` lists, forks and deletes threads, for a session picker:

```go
st, err := filestore.Open("/var/lib/myapp/sessions", filestore.Options{Sync: filestore.SyncAlways})
if err != nil {
	return err
}
defer st.Close()

ss := st.Sessions()
list, _ := ss.List(ctx, "alice", 20)                  // most recently updated first; limit <= 0: all
fork, _ := ss.Fork(ctx, "alice", list[0].ID, "")      // copy up to a message id; "" copies everything
_ = ss.Delete(ctx, "alice", fork.ID)                  // records are dropped at the next compaction
```

`SessionInfo` has `ID`, `Title`, `CreatedAt`, `UpdatedAt`, `Pinned`, `Preview` (the start of the last user or assistant message), `Messages`, `ActiveRun` and `LastStatus`. A fork never stops between an assistant tool call and its results, and does not copy summaries, goals or approvals. To resume a conversation, keep using its thread id with `Send`; old runs replay with `Events(ctx, oldRunID, 0)`. The `hosts/client` `Sessions` method returns `ErrNotSupported` on a store that is not a filestore.

## pgstore: Postgres

`pgstore.NewPG(pool)` is the Postgres implementation of `harness.Store`. Apply the embedded SQL migrations before use:

```go
pool, err := pgstore.NewPool(ctx, cfg.PostgresDSN(), cfg.DBPoolMaxConns, cfg.PingDBAtStartup)
if err != nil {
	return err
}
defer pool.Close()
if err := migrate.Run(ctx, pool, cfg.PostgresSchema); err != nil { // idempotent; safe from every process
	return err
}
st := pgstore.NewPG(pool)
```

`migrate.Run` creates the schema if it is missing and applies pending migrations, each in its own transaction, under a Postgres advisory lock, so several processes (web replicas, workers) can start together. The configuration comes from `POSTGRES_HOST`, `POSTGRES_PORT`, `POSTGRES_DB`, `POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_SCHEMA` (default `work`), `POSTGRES_SSLMODE` (default `require`) and `DB_POOL_MAX_CONNS` (default 20); see [Environment variables](/docs/reference/env-vars/). `harness-web` (team mode) and `harness-worker` do exactly this at startup.

Postgres is the store for [several processes](/docs/guides/postgres-redis/): with it, events are written to a table and, together with [Redis](/docs/concepts/infra/), tailed live from a stream. Artifact contents default to the same database (`work_blobs`); `Options.Blobs` can point them at S3-compatible storage instead.

## Retention

Deleting a thread is a soft delete; the retention job hard-deletes it later.

| Variable | Default | Meaning |
|---|---|---|
| `HARNESS_PURGE_AFTER_DAYS` | `30` | Deleted threads are hard-deleted this many days after deletion, with their runs, messages, events, artifacts, goals and feedback. `<= 0` disables the purge. |
| `HARNESS_BLOB_GC_GRACE_HOURS` | `24` | A content blob is only deleted after it has had no artifact version referencing it for this long. |
| `APPROVAL_TTL_HOURS` | `72` | A pending approval older than this expires (a job runs every minute). |
| `HARNESS_MEMORY_DELETED_RETENTION_DAYS` | `30` | Deleted memory entries are purged after this many days. |

<div class="callout warning">

**Retention runs inside `Harness.Run`.** The purge is a job of the runtime's sweeper, and the `artifacts` feature registers it. A process that never calls `Run` (a web tier that only calls `Send`) does not purge; a deployment with workers purges from the workers, one at a time. Without the `artifacts` feature (`ProfileMinimal`) there is no purge job. In the memory store, a purge only matters for the lifetime of the process.

</div>

Everything persists until deleted: threads, messages and artifacts are kept as long as the thread exists. To delete a user, delete their threads with the store's `DeleteThread` (it also ends their sandboxes), as described in the [production checklist](/docs/operations/production/).

## Writing your own store

Implement `harness.Store` (that is, `store.Full`, `sandbox.Store` and `store.WorkspaceGitStore`), or embed `memstore.Store` and override what you persist, the way `filestore` does. The interface comments in `core/store` are the contract; the ones around runs matter most: `CreateRun` enforces one active run per thread (`store.ErrRunActive`), `ClaimRun` fails with `store.ErrNotClaimable` for a run that is not queued, and `FinishRun` fails with `store.ErrNotOwned` when the run is no longer running on that runtime.

## Related

- [Stores](/docs/integrations/stores/): configuring each backend.
- [Infrastructure](/docs/concepts/infra/): the event log, queue and locks that sit beside the store.
- [Threads, runs and events](/docs/concepts/threads-runs-events/): the data the store holds.
