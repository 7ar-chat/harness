-- Roles: named agent configurations of a project or a user (a prompt, a model, tool limits, skills), with the
-- last versions of each. The document is jsonb so a new field needs no migration; the columns are what is looked
-- up.
CREATE TABLE IF NOT EXISTS work_roles (
    scope      text NOT NULL CHECK (scope IN ('project', 'user')),
    -- scope_id is the project id, or the user's login.
    scope_id   text NOT NULL,
    name       text NOT NULL,
    version    integer NOT NULL,
    doc        jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (scope, scope_id, name)
);

CREATE TABLE IF NOT EXISTS work_role_versions (
    scope      text NOT NULL,
    scope_id   text NOT NULL,
    name       text NOT NULL,
    version    integer NOT NULL,
    doc        jsonb NOT NULL,
    changed_by text NOT NULL DEFAULT '',
    at         timestamptz NOT NULL,
    PRIMARY KEY (scope, scope_id, name, version),
    FOREIGN KEY (scope, scope_id, name) REFERENCES work_roles (scope, scope_id, name) ON DELETE CASCADE
);
