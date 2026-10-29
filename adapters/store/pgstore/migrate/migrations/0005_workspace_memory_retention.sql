-- Harness v2 review fixes: auto titles of untitled threads, workspace
-- materialization tracking, pending memories, blob garbage collection.

-- Threads that existed before 0004 without a title yet get their title from
-- the first message like new ones (0004 defaulted every existing row to 'user').
UPDATE work_threads SET title_source = 'auto' WHERE title = '' AND title_source = 'user';

-- The sandbox in which an artifact's latest version is known to be present
-- (seen in a /workspace listing or written there). The deletion pass of a
-- snapshot only considers artifacts materialized in the snapshotted sandbox,
-- so files a restore skipped (limits, failures) or uploads whose sandbox write
-- failed are never taken for deletions.
ALTER TABLE work_artifacts ADD COLUMN IF NOT EXISTS sandbox_id text;
-- Set when a workspace restore could not write the file; such files go first next time.
ALTER TABLE work_artifacts ADD COLUMN IF NOT EXISTS restore_skipped_at timestamptz;
-- Until now every live artifact was assumed to be in the thread's current sandbox.
UPDATE work_artifacts a SET sandbox_id = t.sandbox_id
    FROM work_threads t
    WHERE t.id = a.thread_id AND a.deleted_at IS NULL AND a.sandbox_id IS NULL AND t.sandbox_id IS NOT NULL;

-- Memories proposed by the model after untrusted content wait for the user's confirmation.
ALTER TABLE work_memories ADD COLUMN IF NOT EXISTS status text NOT NULL DEFAULT 'active';
ALTER TABLE work_memories DROP CONSTRAINT IF EXISTS work_memories_status_check;
ALTER TABLE work_memories ADD CONSTRAINT work_memories_status_check CHECK (status IN ('active', 'pending'));

-- Retention: threads soft-deleted more than WORK_PURGE_AFTER_DAYS ago are
-- hard-deleted by the runtime sweeper; the content blobs of their artifact
-- versions are queued here and deleted once no artifact version references them.
CREATE TABLE IF NOT EXISTS work_blob_gc (
    key        text        PRIMARY KEY,
    queued_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS work_artifact_versions_sha256_idx ON work_artifact_versions (sha256);

CREATE INDEX IF NOT EXISTS work_threads_deleted_idx ON work_threads (deleted_at) WHERE deleted_at IS NOT NULL;

-- Completed context compactions, so the "Context compacted" divider survives a
-- reload (shown after after_message_id; hidden with that message by an edit).
CREATE TABLE IF NOT EXISTS work_compactions (
    id               uuid        PRIMARY KEY,
    thread_id        uuid        NOT NULL REFERENCES work_threads (id) ON DELETE CASCADE,
    run_id           uuid        REFERENCES work_runs (id) ON DELETE SET NULL,
    after_message_id text        NOT NULL,
    trigger          text        NOT NULL CHECK (trigger IN ('auto', 'manual')),
    tokens_before    integer     NOT NULL,
    tokens_after     integer     NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (thread_id, after_message_id) REFERENCES work_messages (thread_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS work_compactions_thread_idx ON work_compactions (thread_id, created_at);
