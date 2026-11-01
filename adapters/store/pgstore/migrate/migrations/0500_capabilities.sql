-- The MCP marketplace: the catalog of servers the product ships (a fixture,
-- seeded at startup) and the installs of a project or a user: an override of a
-- catalog entry (on/off, filled-in fields, narrowed tools) or a custom server.
CREATE TABLE IF NOT EXISTS work_mcp_catalog (
    id         text PRIMARY KEY,
    entry      jsonb NOT NULL,
    -- builtin marks entries that came from the shipped fixture; seeding only
    -- touches those.
    builtin    boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS work_mcp_installs (
    id          text PRIMARY KEY,
    scope       text NOT NULL CHECK (scope IN ('project', 'user')),
    -- scope_id is the project id, or the user's login.
    scope_id    text NOT NULL,
    -- catalog_id names the entry a catalog install configures; '' for a custom server.
    catalog_id  text NOT NULL DEFAULT '',
    name        text NOT NULL,
    url         text NOT NULL DEFAULT '',
    transport   text NOT NULL DEFAULT '',
    tools       jsonb NOT NULL DEFAULT '[]',
    enabled     boolean NOT NULL DEFAULT false,
    -- secrets is the sealed JSON of what the user filled in; secret_keys names what is set.
    secrets     bytea NOT NULL DEFAULT '',
    secret_keys jsonb NOT NULL DEFAULT '[]',
    created_by  text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS work_mcp_installs_catalog_idx ON work_mcp_installs (scope, scope_id, catalog_id) WHERE catalog_id <> '';
CREATE UNIQUE INDEX IF NOT EXISTS work_mcp_installs_name_idx ON work_mcp_installs (scope, scope_id, name);
