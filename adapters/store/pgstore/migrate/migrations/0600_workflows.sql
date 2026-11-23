-- Workflows: the procedures (SOPs) of a project as a versioned graph, the last versions of each, and the runs
-- that carry one out. The documents are jsonb so a new field needs no migration; the columns are what is looked
-- up.
CREATE TABLE IF NOT EXISTS work_workflows (
    id         text PRIMARY KEY,
    project_id text NOT NULL,
    name       text NOT NULL,
    version    integer NOT NULL,
    doc        jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (project_id, name)
);

CREATE TABLE IF NOT EXISTS work_workflow_versions (
    workflow_id text NOT NULL REFERENCES work_workflows (id) ON DELETE CASCADE,
    version     integer NOT NULL,
    doc         jsonb NOT NULL,
    changed_by  text NOT NULL DEFAULT '',
    at          timestamptz NOT NULL,
    PRIMARY KEY (workflow_id, version)
);

CREATE TABLE IF NOT EXISTS work_workflow_runs (
    id            text PRIMARY KEY,
    project_id    text NOT NULL,
    workflow_id   text NOT NULL,
    parent_run_id text NOT NULL DEFAULT '',
    state         text NOT NULL,
    version       integer NOT NULL,
    doc           jsonb NOT NULL,
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS work_workflow_runs_project ON work_workflow_runs (project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS work_workflow_runs_active ON work_workflow_runs (state) WHERE state IN ('running', 'paused');
