-- Recipes: ready-made, parameterized schedules, tasks and triggers that an instance, a project or a person shares,
-- with the last versions of each and a use counter (builtin recipes are code; only their counter is stored). The
-- document is jsonb so a new field needs no migration; the columns are what is looked up.
CREATE TABLE IF NOT EXISTS work_recipes (
    id         text PRIMARY KEY,
    scope      text NOT NULL CHECK (scope IN ('instance', 'project', 'personal')),
    -- scope_id is the project id of a project recipe, the author's login of a personal one, '' for the instance.
    scope_id   text NOT NULL,
    version    integer NOT NULL,
    doc        jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS work_recipes_scope ON work_recipes (scope, scope_id);

CREATE TABLE IF NOT EXISTS work_recipe_versions (
    id         text NOT NULL REFERENCES work_recipes (id) ON DELETE CASCADE,
    version    integer NOT NULL,
    doc        jsonb NOT NULL,
    changed_by text NOT NULL DEFAULT '',
    at         timestamptz NOT NULL,
    PRIMARY KEY (id, version)
);

CREATE TABLE IF NOT EXISTS work_recipe_uses (
    id   text PRIMARY KEY,
    uses integer NOT NULL DEFAULT 0
);
