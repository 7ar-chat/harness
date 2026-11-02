---
title: Stores
description: "Pick where threads, runs, messages and artifacts persist: memory, a journal on disk, Postgres, a team store, or S3 for blobs."
---

The store holds the durable state of the harness: threads, messages, runs, event logs, approvals and more. `harness.New` defaults to memory; pass `Options.Store` to keep state across restarts or share it between processes. The port itself is described in [The store](/docs/concepts/store/); this page covers the adapters that ship.

| | `memstore` | `filestore` | `pgstore` | `teamstore` | `s3blobs` |
|---|---|---|---|---|---|
| Holds | everything | threads, messages, runs, event logs, approvals, goals, feedback, summaries, memory entries | everything | users, projects, members, invites, connectors, audit | artifact contents only |
| Survives restart | no | yes | yes | `Mem` with a file: yes; `PG`: yes | yes |
| Processes | one | one (directory lock) | many | like its backing store | many |
| Needs | nothing | a directory | Postgres | none, a file or Postgres | an S3-compatible bucket |
| Wire it with | default | `filestore.Open(dir)` | `pgstore.NewPG(pool)` | `teamstore.Open(path)` or `NewPG(pool)` | `Options.Blobs` |

Pick by [deployment shape](/docs/guides/deployment-shapes/): memory for tests and demos, `filestore` for a terminal or one node, Postgres (plus [Redis](/docs/integrations/redis/)) for several processes.

## memstore

`memstore.New()` is a goroutine-safe in-memory `store.Full`. Everything is lost on exit. It is what you get when `Options.Store` is nil, and it mirrors Postgres behaviour and constraints (unique keys, soft deletes) so tests written against it hold on `pgstore`.

## filestore

`filestore` wraps `memstore` and persists the conversation state to an append-only JSON-lines journal, `journal.jsonl`, in a state directory. No Postgres, no Redis, no cgo.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/filestore"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/agui"
)

func open(dir string) (*harness.Harness, *filestore.Store) {
	os.Setenv("LLM_MODELS", "m")
	os.Setenv("LLM_DEFAULT_MODEL", "m")
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	// SyncAlways fsyncs every batch; the default SyncInterval fsyncs every 500 ms and on Close.
	st, err := filestore.Open(dir, filestore.Options{Sync: filestore.SyncAlways})
	if err != nil {
		log.Fatal(err)
	}
	model := llmtest.New(llmtest.Turn{Chunks: llmtest.Text("hello from the journal")})
	h, err := harness.New(harness.Options{Config: cfg, Store: st, Models: llmtest.Factory{M: model}})
	if err != nil {
		log.Fatal(err)
	}
	return h, st
}

func main() {
	dir, _ := os.MkdirTemp("", "state")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h, st := open(dir)
	go func() { _ = h.Run(ctx) }()
	th, _ := h.NewThread(ctx, "alice", "demo")
	run, err := h.Send(ctx, "alice", th.ID, "hi", harness.RunOptions{Model: "m", Sandbox: harness.SandboxOff})
	if err != nil {
		log.Fatal(err)
	}
	for ev := range h.Events(ctx, run.ID, 0) {
		if agui.IsTerminal(ev.Type) {
			break
		}
	}
	if err := st.Close(); err != nil {
		log.Fatal(err)
	}

	// A second open of the same directory replays the journal.
	_, st2 := open(dir)
	defer st2.Close()
	sessions, _ := st2.Sessions().List(ctx, "alice", 10)
	for _, s := range sessions {
		fmt.Println(s.Title, s.Messages, s.LastStatus)
	}

	// The directory is locked while a store has it open.
	if _, err := filestore.Open(dir); err != nil {
		fmt.Println(err)
	}
}
```

Output:

```text
demo 2 completed
filestore: state directory is in use by another process: resource temporarily unavailable
```

`harness.OpenLocalStore(dir)` is the shortcut hosts use; an empty `dir` resolves to `$LOCAL_STATE_DIR`, else `<user dir>/sessions` (`$HAR_HOME` or `~/.har`), else `$XDG_STATE_HOME/harness`, else `~/.local/state/harness`.

### Journal and durability

Reads are served from memory. Every mutating call is applied to memory first, then the entities it changed are appended to the journal as idempotent upserts; unchanged records are skipped by content hash. Each batch is one `write(2)` on an `O_APPEND` file, so a process crash (even `kill -9`) loses nothing and can at most tear the last line, which `Open` detects and truncates. A bad line anywhere else is reported as `ErrCorrupt` rather than skipped.

| `Options.Sync` | fsync | Loses on power failure |
|---|---|---|
| `SyncInterval` (default) | at most every 500 ms (`SyncEvery` overrides), and on `Close` | the last window |
| `SyncAlways` | every batch | nothing acknowledged |
| `SyncNever` | left to the OS | whatever the OS had not flushed |

On `Open`, every run that was queued or running when the previous process died is marked `interrupted` and its event log gets a `RUN_ERROR` so replays terminate. The journal is rewritten (temp file, atomic rename) when it holds many superseded records or deleted threads.

### Lock

`Open` takes an exclusive advisory `flock` on `LOCK` in the directory, so two processes never append to one journal. The lock dies with the process. On non-Unix platforms the lock is a no-op: do not share the directory.

### What persists

Threads (title, pin, plan, pending question), messages, runs, event logs, approvals, goals, feedback, summaries, compactions and memory entries are journaled. Artifacts, blobs, workspace git archives and sandbox ids stay in memory, and are rebuilt or recreated on demand.

<div class="callout note">

**One process only.** `filestore` is for a terminal, a desktop app or a single node. For several processes use Postgres and Redis.

</div>

## pgstore

`pgstore.NewPG(pool)` is the Postgres `store.Full` for several processes and production.

```go
pool, err := pgstore.NewPool(ctx, cfg.PostgresDSN(), cfg.DBPoolMaxConns, cfg.PingDBAtStartup)
if err != nil {
	log.Fatal(err)
}
defer pool.Close()
if err := migrate.Run(ctx, pool, cfg.PostgresSchema); err != nil { // adapters/store/pgstore/migrate
	log.Fatal(err)
}
store := pgstore.NewPG(pool)
```

The complete wiring, with Redis, is in [Redis](/docs/integrations/redis/#wiring-example).

| Variable | Default |
|---|---|
| `POSTGRES_HOST` | `postgres` |
| `POSTGRES_PORT` | `5432` |
| `POSTGRES_DB` | `db` |
| `POSTGRES_USER` | `postgres` |
| `POSTGRES_PASSWORD` | empty |
| `POSTGRES_SCHEMA` | `work` |
| `POSTGRES_SSLMODE` | `require` |
| `DB_POOL_MAX_CONNS` | `20` |
| `PING_DB_AT_STARTUP` | `true` |

`cfg.PostgresDSN()` builds the URL with `search_path` set to the schema, `sslmode` and `application_name=harness`.

<div class="callout warning">

**`POSTGRES_SSLMODE` defaults to `require`.** A local Postgres without TLS needs `POSTGRES_SSLMODE=disable` or the connection fails.

</div>

### Migrations

`migrate.Run(ctx, db, schema)` creates the schema if missing and applies the embedded SQL files in name order, each in its own transaction, recording them in `work_migrations`. A Postgres advisory lock serialises concurrent migrators of one schema, so several replicas can call it at start. The stock `harness-web` (team mode with `POSTGRES_HOST`) and `harness-worker` run it for you.

### Tables

All tables carry the `work_` prefix.

| Family | Tables |
|---|---|
| Conversation | `work_threads`, `work_runs`, `work_messages`, `work_run_events`, `work_compactions`, `work_goals`, `work_message_feedback`, `work_approvals`, `work_thread_seen`, `work_user_settings` |
| Artifacts and blobs | `work_artifacts`, `work_artifact_versions`, `work_blobs`, `work_blob_gc`, `work_renders`, `work_workspace_git`, `work_workspace_git_issues` |
| Memory | `work_memory_entries`, `work_memory_embeddings` |
| Team | `work_users`, `work_projects`, `work_project_members`, `work_project_invites`, `work_thread_meta`, `work_audit`, `work_user_avatars`, `work_connectors` |
| Bookkeeping | `work_migrations` |

### Testing against Postgres

The pgstore and migration tests are skipped unless `HARNESS_TEST_DATABASE_URL` points at a Postgres, for example `postgres://harness:harness@localhost:5432/harness?sslmode=disable` (the CI value). Each test works in a schema of its own and drops it afterwards. See [Test your agent](/docs/guides/testing/).

## teamstore

`teamstore` implements `store.TeamStore` for [team mode](/docs/guides/team-mode/): users, projects with member roles, invites, thread-to-project links, the audit trail, avatars, connectors (encrypted tokens) and per-user "last seen".

- `teamstore.NewMem()` is in memory.
- `teamstore.Open(path)` is the same, written to a JSON file on every change (atomically, by rename): `harness-web -team` without Postgres keeps `team.json` beside the sessions.
- `teamstore.NewPG(pool)` uses the team tables of the same Postgres.

In `harness-web -team`, `POSTGRES_HOST` selects Postgres for both stores. Without it, sessions are kept by `filestore` and the team data in `team.json` next to them.

## Blobs and artifacts

Artifact contents are content-addressed (sha256, deduplicated) blobs behind `Options.Blobs`. The default stores them in the main store: `work_blobs` in Postgres, capped at 50 MiB per blob. For larger files or cheaper storage, use an S3-compatible bucket (AWS, RustFS, Dell ECS) through `s3blobs`, which streams uploads (multipart above 16 MiB):

```go
blobs, err := s3blobs.New(s3blobs.Options{
	Bucket:    cfg.ArtifactsS3Bucket,
	Endpoint:  cfg.ArtifactsS3Endpoint,
	Region:    cfg.ArtifactsS3Region,
	AccessKey: cfg.AWSAccessKeyID,
	SecretKey: cfg.AWSSecretAccessKey,
	PathStyle: cfg.ArtifactsS3PathStyle,
	Prefix:    cfg.ArtifactsS3Prefix,
})
if err != nil {
	log.Fatal(err) // "ARTIFACTS_S3_BUCKET is empty" without a bucket
}
if cfg.ArtifactsS3CreateBucket {
	if err := blobs.EnsureBucket(ctx); err != nil {
		log.Fatal(err)
	}
}
h, err := harness.New(harness.Options{Config: cfg, Store: store, Blobs: blobs})
```

| Variable | Default |
|---|---|
| `ARTIFACTS_S3_BUCKET` | empty |
| `ARTIFACTS_S3_ENDPOINT` | empty (AWS default) |
| `ARTIFACTS_S3_REGION` | `us-east-1` |
| `ARTIFACTS_S3_PATH_STYLE` | `true` |
| `ARTIFACTS_S3_PREFIX` | `harness/` |
| `ARTIFACTS_S3_CREATE_BUCKET` | `false` |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | empty |

<div class="callout note">

**The S3 variables are parsed but not wired.** `config.Load` reads `ARTIFACTS_S3_*`, but none of the stock binaries builds an S3 blob store from them: `harness-web` and `harness-worker` use the main store's blobs. To use S3 you pass `Options.Blobs` yourself, as above. Unreferenced blobs are collected after `HARNESS_BLOB_GC_GRACE_HOURS` (default 24); deleted threads are purged after `HARNESS_PURGE_AFTER_DAYS` (default 30).

</div>

The S3 blob tests are skipped unless `HARNESS_TEST_S3_ENDPOINT` (with `_ACCESS_KEY` and `_SECRET_KEY`) is set. For how artifacts are exposed, see [Artifacts and checkpoints](/docs/concepts/artifacts-checkpoints/).
