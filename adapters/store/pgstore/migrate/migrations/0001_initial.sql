CREATE TABLE IF NOT EXISTS work_threads (
    id          uuid PRIMARY KEY,
    user_login  text        NOT NULL,
    title       text        NOT NULL DEFAULT '',
    pinned_at   timestamptz,
    sandbox_id  text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    deleted_at  timestamptz
);

-- Thread list: pinned first, newest first, per user, excluding deleted.
CREATE INDEX IF NOT EXISTS work_threads_user_list_idx
    ON work_threads (user_login, (pinned_at IS NOT NULL) DESC, updated_at DESC, id DESC)
    WHERE deleted_at IS NULL;

-- Per-user sandbox quota.
CREATE INDEX IF NOT EXISTS work_threads_user_sandbox_idx
    ON work_threads (user_login)
    WHERE sandbox_id IS NOT NULL AND deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS work_runs (
    id               uuid PRIMARY KEY,
    thread_id        uuid        NOT NULL REFERENCES work_threads (id) ON DELETE CASCADE,
    user_login       text        NOT NULL,
    status           text        NOT NULL
        CHECK (status IN ('queued', 'running', 'completed', 'failed', 'cancelled', 'interrupted')),
    model            text        NOT NULL,
    error            text,
    cancel_requested boolean     NOT NULL DEFAULT false,
    runtime_id       text,
    heartbeat_at     timestamptz,
    last_seq         bigint      NOT NULL DEFAULT 0,
    -- raw X-Userinfo of the caller, forwarded to the LLM gateway / MCP servers by the runtime
    userinfo         text        NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now(),
    started_at       timestamptz,
    finished_at      timestamptz
);

-- At most one queued/running run per thread.
CREATE UNIQUE INDEX IF NOT EXISTS work_runs_one_active_per_thread
    ON work_runs (thread_id)
    WHERE status IN ('queued', 'running');

CREATE INDEX IF NOT EXISTS work_runs_status_idx
    ON work_runs (status, created_at)
    WHERE status IN ('queued', 'running');

CREATE TABLE IF NOT EXISTS work_messages (
    -- client-supplied ids are only unique within their thread (no cross-user existence oracle)
    id           text        NOT NULL,
    thread_id    uuid        NOT NULL REFERENCES work_threads (id) ON DELETE CASCADE,
    run_id       uuid        REFERENCES work_runs (id) ON DELETE SET NULL,
    position     bigserial   NOT NULL,
    role         text        NOT NULL CHECK (role IN ('user', 'assistant', 'tool', 'reasoning')),
    content      text        NOT NULL DEFAULT '',
    tool_calls   jsonb,
    tool_call_id text,
    error        text,
    metadata     jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (thread_id, id)
);

CREATE INDEX IF NOT EXISTS work_messages_thread_idx ON work_messages (thread_id, position);

CREATE TABLE IF NOT EXISTS work_run_events (
    run_id     uuid        NOT NULL REFERENCES work_runs (id) ON DELETE CASCADE,
    seq        bigint      NOT NULL,
    type       text        NOT NULL,
    payload    jsonb       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, seq)
);
