-- Profile (display name, uploaded avatar) and connectors (GitHub, Telegram) of a user.
ALTER TABLE work_users ADD COLUMN IF NOT EXISTS display_name text NOT NULL DEFAULT '';
ALTER TABLE work_users ADD COLUMN IF NOT EXISTS avatar_version text NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS work_user_avatars (
    login text PRIMARY KEY REFERENCES work_users(login) ON DELETE CASCADE,
    mime  text NOT NULL,
    data  bytea NOT NULL
);

CREATE TABLE IF NOT EXISTS work_connectors (
    login      text NOT NULL REFERENCES work_users(login) ON DELETE CASCADE,
    kind       text NOT NULL,
    account    text NOT NULL,
    label      text NOT NULL DEFAULT '',
    data       bytea,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (login, kind)
);
CREATE UNIQUE INDEX IF NOT EXISTS work_connectors_account_idx ON work_connectors (kind, account);
