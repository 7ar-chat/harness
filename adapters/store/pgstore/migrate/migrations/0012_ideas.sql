-- Ideas (Work's RequestIdeaUsage): which ideas users select in the Ideas panel
-- and submit from the composer. The catalog itself is static (internal/ideas).
CREATE TABLE IF NOT EXISTS work_idea_usage (
    id          uuid PRIMARY KEY,
    user_login  text        NOT NULL,
    -- The conversation; kept as NULL once the thread is purged.
    thread_id   uuid        REFERENCES work_threads (id) ON DELETE SET NULL,
    idea_id     text        NOT NULL CHECK (char_length(idea_id) BETWEEN 1 AND 100),
    action      text        NOT NULL CHECK (action IN ('selected', 'submitted')),
    surface     text        NOT NULL DEFAULT '' CHECK (char_length(surface) <= 64),
    metadata    jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS work_idea_usage_idea_idx ON work_idea_usage (idea_id, action, created_at DESC);
CREATE INDEX IF NOT EXISTS work_idea_usage_user_idx ON work_idea_usage (user_login, created_at DESC);
CREATE INDEX IF NOT EXISTS work_idea_usage_thread_idx ON work_idea_usage (thread_id) WHERE thread_id IS NOT NULL;
