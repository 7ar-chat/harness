-- A person's own small settings document (getting-started progress, folded explorer sections, panel sizes): it follows them
-- across browsers. At most 8 KB of JSON, enforced by the store.
CREATE TABLE IF NOT EXISTS work_user_preferences (
    login      text        PRIMARY KEY REFERENCES work_users(login) ON DELETE CASCADE,
    doc        jsonb       NOT NULL DEFAULT '{}',
    updated_at timestamptz NOT NULL DEFAULT '1970-01-01 00:00:00+00'
);
