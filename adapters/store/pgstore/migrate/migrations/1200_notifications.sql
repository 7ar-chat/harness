-- Notification rules, channels, personal preferences and the delivery log (core/notify). Each row is one jsonb document;
-- the sealed address and secret of a channel or a personal webhook are inside it, encrypted by the server.
CREATE TABLE IF NOT EXISTS work_notify_rules (
    project_id text PRIMARY KEY,
    doc        jsonb NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS work_notify_channels (
    id         text PRIMARY KEY,
    project_id text NOT NULL,
    name       text NOT NULL,
    doc        jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS work_notify_channels_project ON work_notify_channels (project_id);
CREATE TABLE IF NOT EXISTS work_notify_prefs (
    login      text PRIMARY KEY,
    doc        jsonb NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS work_notify_log (
    seq        bigserial PRIMARY KEY,
    project_id text NOT NULL DEFAULT '',
    login      text NOT NULL DEFAULT '',
    doc        jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS work_notify_log_project ON work_notify_log (project_id, seq DESC);
