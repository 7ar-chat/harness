-- Executors: the external coding-agent CLIs (a command template) a project's owner defined. The document is jsonb
-- so a new field needs no migration; the columns are what is looked up. Credentials are not here: they are sealed
-- per user in the capabilities installs.
CREATE TABLE IF NOT EXISTS work_executors (
    project_id text NOT NULL,
    name       text NOT NULL,
    doc        jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (project_id, name)
);
