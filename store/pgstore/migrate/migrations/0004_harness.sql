-- Harness v2 (§4–§9): run kinds, compaction summary, plan, ask_user questions,
-- edit & resend, LLM titles, goals, memory, feedback.

ALTER TABLE work_runs ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'chat';
ALTER TABLE work_runs DROP CONSTRAINT IF EXISTS work_runs_kind_check;
ALTER TABLE work_runs ADD CONSTRAINT work_runs_kind_check CHECK (kind IN ('chat', 'goal', 'compact'));
ALTER TABLE work_runs ADD COLUMN IF NOT EXISTS reasoning_effort text;

-- Existing titles are kept as they are (never replaced by a generated title).
ALTER TABLE work_threads ADD COLUMN IF NOT EXISTS title_source text NOT NULL DEFAULT 'user';
ALTER TABLE work_threads ALTER COLUMN title_source SET DEFAULT 'auto';
ALTER TABLE work_threads DROP CONSTRAINT IF EXISTS work_threads_title_source_check;
ALTER TABLE work_threads ADD CONSTRAINT work_threads_title_source_check CHECK (title_source IN ('auto', 'user', 'generated'));
-- Compaction: summary of the messages up to summary_through_position (work_messages.position).
ALTER TABLE work_threads ADD COLUMN IF NOT EXISTS summary text;
ALTER TABLE work_threads ADD COLUMN IF NOT EXISTS summary_through_position bigint;
-- update_plan items and the open ask_user question.
ALTER TABLE work_threads ADD COLUMN IF NOT EXISTS plan jsonb;
ALTER TABLE work_threads ADD COLUMN IF NOT EXISTS pending_question jsonb;

-- Edit & resend: hidden messages are kept for audit.
ALTER TABLE work_messages ADD COLUMN IF NOT EXISTS superseded_at timestamptz;

CREATE INDEX IF NOT EXISTS work_messages_thread_live_idx
    ON work_messages (thread_id, position)
    WHERE superseded_at IS NULL;

-- One goal per thread (PUT replaces it).
CREATE TABLE IF NOT EXISTS work_goals (
    thread_id     uuid        PRIMARY KEY REFERENCES work_threads (id) ON DELETE CASCADE,
    id            uuid        NOT NULL,
    text          text        NOT NULL,
    status        text        NOT NULL CHECK (status IN ('active', 'met', 'exhausted', 'cleared')),
    turns_used    int         NOT NULL DEFAULT 0 CHECK (turns_used >= 0),
    max_turns     int         NOT NULL CHECK (max_turns BETWEEN 1 AND 20),
    -- {"met": bool, "reason": string, "at": timestamp}
    last_verdict  jsonb,
    eval_failures int         NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (turns_used <= max_turns)
);

CREATE TABLE IF NOT EXISTS work_memories (
    id               uuid        PRIMARY KEY,
    user_login       text        NOT NULL,
    text             text        NOT NULL CHECK (char_length(text) BETWEEN 1 AND 500),
    source_thread_id uuid,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS work_memories_user_idx ON work_memories (user_login, created_at);

CREATE TABLE IF NOT EXISTS work_user_settings (
    user_login     text PRIMARY KEY,
    memory_enabled boolean     NOT NULL DEFAULT true,
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS work_message_feedback (
    thread_id  uuid        NOT NULL,
    message_id text        NOT NULL,
    user_login text        NOT NULL,
    sentiment  text        NOT NULL CHECK (sentiment IN ('like', 'dislike')),
    comment    text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (thread_id, message_id),
    FOREIGN KEY (thread_id, message_id) REFERENCES work_messages (thread_id, id) ON DELETE CASCADE
);
