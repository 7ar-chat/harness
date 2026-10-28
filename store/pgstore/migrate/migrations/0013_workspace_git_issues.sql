-- Work Chat files: why the thread's checkpoint history (work_workspace_git) is
-- not up to date — the last archive was refused (too large) or failed — so the
-- workspace status can say "history not saved" instead of silently restoring a
-- stale archive. Cleared by the next successful archive; purged with the thread.
CREATE TABLE IF NOT EXISTS work_workspace_git_issues (
    thread_id  uuid        PRIMARY KEY REFERENCES work_threads (id) ON DELETE CASCADE,
    reason     text        NOT NULL,
    head       text        NOT NULL DEFAULT '',
    size_bytes bigint      NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now()
);
