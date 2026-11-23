-- Requirements tracking: what a project wants done, followed from the idea to the merged pull request, and the
-- per-project settings of auto-dispatch.
CREATE TABLE IF NOT EXISTS work_requirements (
    id         text PRIMARY KEY,
    project_id text NOT NULL,
    title      text NOT NULL,
    body       text NOT NULL DEFAULT '',
    status     text NOT NULL DEFAULT 'backlog' CHECK (status IN ('backlog', 'ready', 'in_progress', 'in_review', 'done', 'rejected')),
    priority   integer NOT NULL DEFAULT 0,
    labels     jsonb NOT NULL DEFAULT '[]',
    source     jsonb NOT NULL DEFAULT '{}',
    -- source_key identifies an imported issue inside the project ('' for a manual requirement).
    source_key text NOT NULL DEFAULT '',
    author     text NOT NULL DEFAULT '',
    thread_ids jsonb NOT NULL DEFAULT '[]',
    task_ids   jsonb NOT NULL DEFAULT '[]',
    pr         jsonb NOT NULL DEFAULT '{}',
    flag       text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS work_requirements_project_idx ON work_requirements (project_id, status);
CREATE UNIQUE INDEX IF NOT EXISTS work_requirements_source_idx ON work_requirements (project_id, source_key) WHERE source_key <> '';

CREATE TABLE IF NOT EXISTS work_requirement_settings (
    project_id text PRIMARY KEY,
    settings   jsonb NOT NULL
);
