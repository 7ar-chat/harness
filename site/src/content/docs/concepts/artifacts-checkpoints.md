---
title: Artifacts and checkpoints
description: "How files the agent writes in /workspace become durable, versioned artifacts, and how per-thread Git checkpoints let you diff and restore the workspace across sandbox restarts."
---

A sandbox is disposable, so the harness saves what matters. The **artifacts** feature (`features/artifacts`) snapshots the files the agent produces under `/workspace` into a content-addressed blob store, with a version per change. The **workspace checkpoints** feature (`features/artifacts/workspace`) keeps a private Git history of `/workspace` per thread, with per-file history, diff and restore. Both are part of the default `work` and `coding` profiles; checkpoints require artifacts. See [Sandboxes](/docs/concepts/sandboxes/) for the sandbox they run in.

## Artifacts in a run

This program lets a scripted model write a file with `write_file`, then lists the saved artifact and the checkpoint history. It uses the in-memory fake sandbox with a real shell (`ShellExec`) for the Git checkpoints, so it needs `git` and `bash` on your machine, and runs offline:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

func main() {
	os.Setenv("LLM_PROVIDER", "openai")
	os.Setenv("LLM_MODELS", "m")
	os.Setenv("LLM_DEFAULT_MODEL", "m")
	os.Setenv("SANDBOX_BACKEND", "none")
	cfg, err := config.Load()
	check(err)

	// The model writes a report into /workspace/outputs, then says it is done.
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		for _, m := range c.Messages {
			if m.Role == ai.RoleTool {
				return llmtest.Turn{Chunks: llmtest.Text("Saved.")}
			}
		}
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", "write_file",
			`{"path":"/workspace/outputs/report.md","content":"# Q3\n"}`), "tool_calls")}}
	})
	// FindExec answers the workspace listing the snapshot runs; ShellExec runs real bash (git) for the checkpoints.
	backend := sandboxtest.NewBackend()
	backend.OnCreate = func(sb *sandboxtest.Sandbox) { sb.SetExec(sandboxtest.FindExec(sb, sandboxtest.ShellExec(sb))) }
	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: model}, Backend: backend})
	check(err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	th, err := h.NewThread(ctx, "alice", "")
	check(err)
	run, err := h.Send(ctx, "alice", th.ID, "write the Q3 report", harness.RunOptions{Model: "m"})
	check(err)
	for range h.Events(ctx, run.ID, 0) { // wait for RUN_FINISHED
	}
	h.Workspace.WaitArchives(ctx)

	arts, err := h.Artifacts.ListArtifacts(ctx, th.ID)
	check(err)
	for _, a := range arts {
		data, v, err := h.Artifacts.Content(ctx, a, 0)
		check(err)
		fmt.Printf("%s v%d %d bytes %s sha256:%.8s %q\n", a.Path, v.Version, a.SizeBytes, a.MimeType, a.SHA256, data)
	}

	// The workspace checkpoints: one git commit per AI turn, listed per file.
	cps, err := h.Workspace.List(ctx, th.ID, "")
	check(err)
	for _, c := range cps {
		fmt.Println("checkpoint:", c.Kind, c.Label, c.ChangedFiles)
	}
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
```

Output:

```text
/workspace/outputs/report.md v1 5 bytes text/markdown sha256:304d7413 "# Q3\n"
checkpoint: automatic Initial workspace checkpoint []
```

The artifact is saved at version 1 with its content hash. The only checkpoint is the initial one, taken before the file existed; the file is in the next turn's checkpoint.

## How artifacts are saved

* **When.** After tool calls that change files, and again when a run ends (any status; a `compact` run takes no snapshot). Each change is announced to clients with a `work.artifact` event; a file that could not be saved arrives as `work.artifact_skipped`. See [Event types](/docs/reference/events/).
* **What.** The files under `/workspace`, except `/workspace/.work` (scratch), and anything inside `node_modules`, `.git`, `__pycache__` or `.venv`. Final deliverables belong in `/workspace/outputs/`.
* **How.** Content is stored once per SHA-256 in the `Blobs` store and each change adds a version to the artifact row. A file that disappears from the workspace is marked deleted.
* **Limits** (`artifacts.Options`): files above 50 MiB are skipped, at most 500 files per snapshot (newest first). A blob backend with its own cap lowers the file limit: the Postgres store caps a blob at 50 MiB, S3 has no cap.
* **Restore.** When a sandbox is created for a thread that already has artifacts, the latest version of each non-deleted file is written back before anyone can use the sandbox, at most 200 files and 500 MiB (files that did not fit stay pending and go first next time). The model receives a notice that lists what was restored. Packages and processes are not restored. With the `local` backend nothing is restored: the host keeps its own files.

## Blob storage

The default blob store keeps contents in the main store (`artifacts.PGBlobs` over Postgres, or in memory). For larger files and shared deployments, pass an S3-compatible store as `Options.Blobs`. The package `adapters/blobs/s3blobs` implements it for AWS, Dell ECS and RustFS, and lives apart so the AWS SDK is linked only by hosts that use it:

```go
import (
	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/blobs/s3blobs"
)

blobs, err := s3blobs.New(s3blobs.Options{
	Bucket:    cfg.ArtifactsS3Bucket,
	Endpoint:  cfg.ArtifactsS3Endpoint,
	Region:    cfg.ArtifactsS3Region,
	AccessKey: cfg.AWSAccessKeyID,
	SecretKey: cfg.AWSSecretAccessKey,
	PathStyle: cfg.ArtifactsS3PathStyle,
	Prefix:    cfg.ArtifactsS3Prefix,
})
// err is "ARTIFACTS_S3_BUCKET is empty" without a bucket.
h, err := harness.New(harness.Options{Config: cfg, Blobs: blobs})
```

(This was compiled against the repository and reaches the error for an empty bucket; it was not run against a live S3.)

<div class="callout note">

**The S3 variables are configuration, not wiring.** `config.Load` parses `ARTIFACTS_S3_BUCKET`, `ARTIFACTS_S3_ENDPOINT`, `ARTIFACTS_S3_REGION` (default `us-east-1`), `ARTIFACTS_S3_PATH_STYLE` (default `true`), `ARTIFACTS_S3_PREFIX` (default `harness/`), `ARTIFACTS_S3_CREATE_BUCKET`, `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, but `harness.New` and the bundled binaries do not read them: the host builds `s3blobs` as above. `Blobs.EnsureBucket(ctx)` creates a missing bucket, which is what `ARTIFACTS_S3_CREATE_BUCKET` is meant for.

</div>

Implement `core/blobs.Blobs` (Put, Get, Has, Delete, MaxBytes) to store contents anywhere else; an optional `PutStream` makes large uploads stream.

## Uploads and limits

User files and image attachments are stored as artifacts too, at the workspace root (`/workspace/<name>`, `name (2).ext` if taken), or at a validated `TargetPath` under `/workspace`. Names are sanitised, and for known types (PDF, Office, images, zip) the extension, declared MIME type and file signature must agree (`ErrTypeMismatch`). `RunOptions.Attachments` accepts images only: at most 6 per message and 5 MiB each (`harness.MaxAttachments`, `harness.MaxAttachmentBytes`).

| Variable | Default | Meaning |
|---|---|---|
| `HARNESS_MAX_UPLOAD_BYTES` | `104857600` | Largest accepted upload for an HTTP host. |
| `HARNESS_MAX_CONCURRENT_UPLOADS` | `4` | Concurrent uploads per process. |
| `HARNESS_MAX_CONCURRENT_UPLOADS_PER_USER` | `4` | Concurrent uploads per user and process. |
| `PREVIEW_MAX_CONCURRENCY` | `2` | Parallel Office to PDF conversions per process. |

These are read into `config.Config` for hosts that serve uploads; the library API (`Service.Upload`) applies the blob store's own cap instead. Previews of Office files need LibreOffice in the sandbox image.

## Workspace checkpoints

With `HARNESS_WORKSPACE_CHECKPOINTS=true` (default), the harness keeps a private Git repository in each thread's `/workspace`:

* **Before every AI turn** that uses the sandbox, it commits all changes as `automatic: Before AI turn` (a sandbox created mid-run gets its checkpoint when first used). The agent may also commit; its commits are listed with kind `agent`. The system prompt tells the model the repository is local, to commit with clear messages, and never to push.
* **History, diff, restore.** `Workspace.List(ctx, threadID, path)` returns the checkpoints touching a file (all when the path is empty), `FileDiff` diffs a file between a checkpoint and the working tree, and `RestoreFile` restores one file as it was at a checkpoint, recording the new artifact version.
* **After each run**, the `.git` directory is archived (tar.gz) into the blob store in the background, and restored into every new sandbox of the thread, before the artifact files. `Workspace.Status` and `Workspace.Restore` report and recreate a stopped workspace; a restore is refused while a run is active.
* **Bounds.** `HARNESS_GIT_ARCHIVE_MAX_BYTES` (default 52428800): a larger history is not archived and the previous archive is kept. `HARNESS_WORKSPACE_CHECKPOINT_MAX_FILE_BYTES` (default 20971520, `<= 0` for no limit): larger files stay out of the history.

<div class="callout warning">

**Checkpoints need `git` in the sandbox image.** Without it the checkpoint is skipped with a log line (`git is not installed in the sandbox image; no checkpoints`) and the run continues. The default `OPENSANDBOX_IMAGE` (`python:3.11-slim`) lacks `git`; use the repository's sandbox image or install it. Checkpoints are also off for the `local` backend, where [`/undo` and the git snapshots](/docs/concepts/approvals-permissions/#git-snapshots-and-undo) work on your own repository instead.

</div>

## Retention and blob GC

Deleting a thread soft-deletes it. The runtime sweeper runs a purge job: threads deleted more than `HARNESS_PURGE_AFTER_DAYS` days ago (default 30; `<= 0` disables the purge) are hard-deleted with their artifacts, and content blobs that no artifact version references any more are collected. A blob is only deleted after it has had no reference for `HARNESS_BLOB_GC_GRACE_HOURS` hours (default 24), so content that a concurrent upload or snapshot is about to reference is never collected. See [Infrastructure](/docs/concepts/infra/) for the sweeper.
