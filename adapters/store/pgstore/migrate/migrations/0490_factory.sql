-- The agent factory: tasks that agents work on unattended, their history, the
-- triggers that create them, the webhook deliveries already handled, what each
-- spawned thread is for, and a spend ledger for budget windows.
CREATE TABLE IF NOT EXISTS work_factory_tasks (
    id             text PRIMARY KEY,
    project_id     text NOT NULL DEFAULT '',
    owner          text NOT NULL DEFAULT '',
    title          text NOT NULL DEFAULT '',
    spec           text NOT NULL DEFAULT '',
    role           text NOT NULL DEFAULT '',
    source         jsonb NOT NULL DEFAULT '{}',
    -- source_key repeats source.key so that one partial unique index makes creation idempotent.
    source_key     text NOT NULL DEFAULT '',
    state          text NOT NULL,
    blocked_reason text NOT NULL DEFAULT '',
    error          text NOT NULL DEFAULT '',
    result         text NOT NULL DEFAULT '',
    parent_id      text NOT NULL DEFAULT '',
    depends_on     jsonb NOT NULL DEFAULT '[]',
    labels         jsonb NOT NULL DEFAULT '[]',
    priority       integer NOT NULL DEFAULT 0,
    not_before     timestamptz,
    thread_id      text NOT NULL DEFAULT '',
    run_id         text NOT NULL DEFAULT '',
    model          text NOT NULL DEFAULT '',
    repo           text NOT NULL DEFAULT '',
    base_branch    text NOT NULL DEFAULT '',
    branch         text NOT NULL DEFAULT '',
    pr             jsonb NOT NULL DEFAULT '{}',
    auto_merge     boolean NOT NULL DEFAULT false,
    attempts       integer NOT NULL DEFAULT 0,
    max_attempts   integer NOT NULL DEFAULT 0,
    policy         text NOT NULL DEFAULT '',
    budget         jsonb NOT NULL DEFAULT '{}',
    -- Spend is kept in plain columns so that AddSpend is an atomic SQL increment.
    spent_usd      double precision NOT NULL DEFAULT 0,
    spent_tokens   bigint NOT NULL DEFAULT 0,
    spent_runs     integer NOT NULL DEFAULT 0,
    spent_unpriced integer NOT NULL DEFAULT 0,
    fingerprints   jsonb NOT NULL DEFAULT '[]',
    feedback       text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    finished_at    timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS work_factory_tasks_source_key_idx ON work_factory_tasks (project_id, source_key) WHERE source_key <> '';
CREATE INDEX IF NOT EXISTS work_factory_tasks_order_idx ON work_factory_tasks (priority DESC, created_at, (id COLLATE "C"));
CREATE INDEX IF NOT EXISTS work_factory_tasks_project_idx ON work_factory_tasks (project_id);
CREATE INDEX IF NOT EXISTS work_factory_tasks_owner_idx ON work_factory_tasks (owner);
CREATE INDEX IF NOT EXISTS work_factory_tasks_state_idx ON work_factory_tasks (state);
CREATE INDEX IF NOT EXISTS work_factory_tasks_parent_idx ON work_factory_tasks (parent_id) WHERE parent_id <> '';
CREATE INDEX IF NOT EXISTS work_factory_tasks_thread_idx ON work_factory_tasks (thread_id) WHERE thread_id <> '';

CREATE TABLE IF NOT EXISTS work_factory_events (
    id      bigserial PRIMARY KEY,
    task_id text NOT NULL REFERENCES work_factory_tasks(id) ON DELETE CASCADE,
    kind    text NOT NULL,
    text    text NOT NULL DEFAULT '',
    data    jsonb,
    at      timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS work_factory_events_task_idx ON work_factory_events (task_id, id);

CREATE TABLE IF NOT EXISTS work_factory_spend (
    id         bigserial PRIMARY KEY,
    project_id text NOT NULL DEFAULT '',
    at         timestamptz NOT NULL,
    usd        double precision NOT NULL DEFAULT 0,
    tokens     bigint NOT NULL DEFAULT 0,
    runs       integer NOT NULL DEFAULT 0,
    unpriced   integer NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS work_factory_spend_at_idx ON work_factory_spend (at);
CREATE INDEX IF NOT EXISTS work_factory_spend_project_idx ON work_factory_spend (project_id, at);

CREATE TABLE IF NOT EXISTS work_factory_triggers (
    id            text PRIMARY KEY,
    project_id    text NOT NULL DEFAULT '',
    owner         text NOT NULL DEFAULT '',
    name          text NOT NULL DEFAULT '',
    kind          text NOT NULL CHECK (kind IN ('cron', 'webhook')),
    schedule      text NOT NULL DEFAULT '',
    event         text NOT NULL DEFAULT '',
    match         jsonb NOT NULL DEFAULT '{}',
    template      jsonb NOT NULL DEFAULT '{}',
    enabled       boolean NOT NULL DEFAULT false,
    next_fire_at  timestamptz,
    last_fired_at timestamptz,
    created_at    timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS work_factory_triggers_due_idx ON work_factory_triggers (next_fire_at) WHERE kind = 'cron' AND enabled AND next_fire_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS work_factory_triggers_project_idx ON work_factory_triggers (project_id);

-- Webhook deliveries already handled; seq orders them for the retention prune.
CREATE TABLE IF NOT EXISTS work_factory_deliveries (
    id      text PRIMARY KEY,
    seq     bigserial NOT NULL,
    seen_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS work_factory_deliveries_seq_idx ON work_factory_deliveries (seq);
CREATE INDEX IF NOT EXISTS work_factory_deliveries_seen_idx ON work_factory_deliveries (seen_at);

CREATE TABLE IF NOT EXISTS work_factory_threads (
    thread_id        text PRIMARY KEY,
    login            text NOT NULL DEFAULT '',
    task_id          text NOT NULL DEFAULT '',
    role             text NOT NULL DEFAULT '',
    instructions     text NOT NULL DEFAULT '',
    policy           text NOT NULL DEFAULT '',
    parent_thread_id text NOT NULL DEFAULT '',
    read_only        boolean NOT NULL DEFAULT false,
    repo             text NOT NULL DEFAULT '',
    base_branch      text NOT NULL DEFAULT '',
    branch           text NOT NULL DEFAULT '',
    updated_at       timestamptz NOT NULL DEFAULT now()
);
