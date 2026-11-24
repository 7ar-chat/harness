-- Per-project data with no table of its own: the weekly digest (setting, claimed slots, history with the generated
-- report) and the sealed credentials of connected outside services (TAPD). Secrets are encrypted by the server.
CREATE TABLE IF NOT EXISTS work_digest_settings (
    project_id text PRIMARY KEY,
    doc        jsonb NOT NULL,
    enabled    boolean NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS work_digest_settings_enabled ON work_digest_settings (enabled) WHERE enabled;
-- A row is a claim: the replica that inserts it sends the digest of that slot.
CREATE TABLE IF NOT EXISTS work_digest_slots (
    project_id text NOT NULL,
    slot       text NOT NULL,
    claimed_at timestamptz NOT NULL,
    PRIMARY KEY (project_id, slot)
);
CREATE TABLE IF NOT EXISTS work_digests (
    id         text PRIMARY KEY,
    project_id text NOT NULL,
    created_at timestamptz NOT NULL,
    doc        jsonb NOT NULL,
    html       bytea NOT NULL,
    report     bytea NOT NULL
);
CREATE INDEX IF NOT EXISTS work_digests_project ON work_digests (project_id, created_at DESC);
CREATE TABLE IF NOT EXISTS work_project_secrets (
    project_id text NOT NULL,
    kind       text NOT NULL,
    sealed     bytea NOT NULL,
    PRIMARY KEY (project_id, kind)
);
