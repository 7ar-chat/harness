-- Scheduled agent tasks and their runs, the usage ledger (one row per model of a finished run) and the monthly spend caps.
CREATE TABLE IF NOT EXISTS work_schedules (
    id              text PRIMARY KEY,
    project_id      text NOT NULL DEFAULT '',
    owner           text NOT NULL,
    run_as          text NOT NULL,
    name            text NOT NULL,
    prompt          text NOT NULL,
    cron            text NOT NULL,
    timezone        text NOT NULL DEFAULT 'UTC',
    model           text NOT NULL DEFAULT '',
    policy          text NOT NULL DEFAULT '',
    timeout_minutes integer NOT NULL DEFAULT 30,
    reuse_session   boolean NOT NULL DEFAULT false,
    thread_id       text NOT NULL DEFAULT '',
    enabled         boolean NOT NULL DEFAULT true,
    next_run_at     timestamptz,
    last_run_at     timestamptz,
    last_status     text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS work_schedules_project_idx ON work_schedules (project_id);
CREATE INDEX IF NOT EXISTS work_schedules_owner_idx ON work_schedules (owner) WHERE project_id = '';
CREATE INDEX IF NOT EXISTS work_schedules_due_idx ON work_schedules (next_run_at) WHERE enabled AND next_run_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS work_schedule_runs (
    id             text PRIMARY KEY,
    schedule_id    text NOT NULL REFERENCES work_schedules(id) ON DELETE CASCADE,
    fire_at        timestamptz NOT NULL,
    trigger        text NOT NULL,
    status         text NOT NULL,
    thread_id      text NOT NULL DEFAULT '',
    agent_run_id   text NOT NULL DEFAULT '',
    error          text NOT NULL DEFAULT '',
    started_by     text NOT NULL DEFAULT '',
    started_at     timestamptz NOT NULL,
    finished_at    timestamptz,
    deadline       timestamptz NOT NULL,
    input_tokens   bigint NOT NULL DEFAULT 0,
    output_tokens  bigint NOT NULL DEFAULT 0,
    cache_read     bigint NOT NULL DEFAULT 0,
    cache_write    bigint NOT NULL DEFAULT 0,
    cost_usd       double precision NOT NULL DEFAULT 0,
    unpriced       integer NOT NULL DEFAULT 0
);
-- One run per scheduled tick: replicas that claim the same tick cannot both start it.
CREATE UNIQUE INDEX IF NOT EXISTS work_schedule_runs_tick_idx ON work_schedule_runs (schedule_id, fire_at) WHERE trigger = 'schedule';
CREATE INDEX IF NOT EXISTS work_schedule_runs_history_idx ON work_schedule_runs (schedule_id, started_at DESC);
CREATE INDEX IF NOT EXISTS work_schedule_runs_thread_idx ON work_schedule_runs (thread_id) WHERE thread_id <> '';
CREATE INDEX IF NOT EXISTS work_schedule_runs_open_idx ON work_schedule_runs (deadline) WHERE status = 'running';

CREATE TABLE IF NOT EXISTS work_usage (
    run_id       text NOT NULL,
    model        text NOT NULL,
    thread_id    text NOT NULL DEFAULT '',
    project_id   text NOT NULL DEFAULT '',
    usr          text NOT NULL DEFAULT '',
    source       text NOT NULL DEFAULT 'session',
    source_id    text NOT NULL DEFAULT '',
    input_tokens bigint NOT NULL DEFAULT 0,
    output_tokens bigint NOT NULL DEFAULT 0,
    cache_read   bigint NOT NULL DEFAULT 0,
    cache_write  bigint NOT NULL DEFAULT 0,
    cost_usd     double precision NOT NULL DEFAULT 0,
    unpriced     boolean NOT NULL DEFAULT false,
    at           timestamptz NOT NULL,
    PRIMARY KEY (run_id, model)
);
CREATE INDEX IF NOT EXISTS work_usage_project_idx ON work_usage (project_id, at);
CREATE INDEX IF NOT EXISTS work_usage_user_idx ON work_usage (usr, at);

CREATE TABLE IF NOT EXISTS work_spend_caps (
    scope       text NOT NULL CHECK (scope IN ('project', 'user')),
    scope_id    text NOT NULL,
    monthly_usd double precision NOT NULL,
    updated_at  timestamptz NOT NULL,
    PRIMARY KEY (scope, scope_id)
);
