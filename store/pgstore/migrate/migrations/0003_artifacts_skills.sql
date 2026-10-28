-- Harness v2 §1–§3: skills, artifacts (durable output documents), uploads.

-- Skill names explicitly requested for a run (preloaded into its system prompt).
ALTER TABLE work_runs ADD COLUMN IF NOT EXISTS skills text[];

-- User skill packages (the validated package, re-zipped with canonical paths).
CREATE TABLE IF NOT EXISTS work_skills (
    id          uuid PRIMARY KEY,
    user_login  text        NOT NULL,
    name        text        NOT NULL,
    description text        NOT NULL,
    version     integer     NOT NULL DEFAULT 1,
    sha256      text        NOT NULL,
    package     bytea       NOT NULL,
    files       integer     NOT NULL,
    size_bytes  bigint      NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_login, name)
);

-- Per-user enable/disable toggle (builtin ids are "builtin:<name>", user skills their uuid).
CREATE TABLE IF NOT EXISTS work_skill_settings (
    user_login text    NOT NULL,
    skill_id   text    NOT NULL,
    enabled    boolean NOT NULL,
    PRIMARY KEY (user_login, skill_id)
);

-- Artifact metadata: one row per (thread, path), latest version denormalized.
CREATE TABLE IF NOT EXISTS work_artifacts (
    id          uuid PRIMARY KEY,
    thread_id   uuid        NOT NULL REFERENCES work_threads (id) ON DELETE CASCADE,
    user_login  text        NOT NULL,
    path        text        NOT NULL,
    name        text        NOT NULL,
    mime_type   text        NOT NULL,
    kind        text        NOT NULL CHECK (kind IN ('output', 'upload')),
    size_bytes  bigint      NOT NULL,
    sha256      text        NOT NULL,
    version     integer     NOT NULL,
    run_id      uuid        REFERENCES work_runs (id) ON DELETE SET NULL,
    -- mtime observed in the sandbox for the latest version (change detection)
    source_mtime double precision NOT NULL DEFAULT 0,
    deleted_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (thread_id, path)
);

CREATE INDEX IF NOT EXISTS work_artifacts_thread_idx ON work_artifacts (thread_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS work_artifact_versions (
    artifact_id uuid        NOT NULL REFERENCES work_artifacts (id) ON DELETE CASCADE,
    version     integer     NOT NULL,
    sha256      text        NOT NULL,
    size_bytes  bigint      NOT NULL,
    run_id      uuid        REFERENCES work_runs (id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (artifact_id, version)
);

-- Postgres blob backend (used when ARTIFACTS_S3_BUCKET is empty). Keys are the
-- content sha256, or "preview/<sha256>.pdf" for cached Office → PDF previews.
CREATE TABLE IF NOT EXISTS work_blobs (
    key        text        PRIMARY KEY,
    bytes      bytea       NOT NULL,
    size       bigint      NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
