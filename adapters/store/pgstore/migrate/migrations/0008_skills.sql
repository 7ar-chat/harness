-- Skills parity with Work: versions (draft / published), global skills
-- (operator-published, tagged), idempotent draft commands and guided skill
-- creation sessions (a session is a thread in mode skill_creation).

-- Private skills: display name and the published / latest version.
ALTER TABLE work_skills ADD COLUMN IF NOT EXISTS display_name text NOT NULL DEFAULT '';
ALTER TABLE work_skills ADD COLUMN IF NOT EXISTS published_version_id uuid;
ALTER TABLE work_skills ADD COLUMN IF NOT EXISTS latest_version_id uuid;

-- Global skills (operator / admin published). The effective package is the
-- one of the published version (else the latest) in work_skill_versions.
CREATE TABLE IF NOT EXISTS work_global_skills (
    id                   uuid PRIMARY KEY,
    name                 text        NOT NULL,
    display_name         text        NOT NULL DEFAULT '',
    description          text        NOT NULL,
    tags                 text[]      NOT NULL DEFAULT '{}',
    published_version_id uuid,
    latest_version_id    uuid,
    created_by           text        NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS work_global_skills_name_idx ON work_global_skills (name);

-- Immutable package versions of private (work_skills) or global skills.
CREATE TABLE IF NOT EXISTS work_skill_versions (
    id               uuid PRIMARY KEY,
    private_skill_id uuid REFERENCES work_skills (id) ON DELETE CASCADE,
    global_skill_id  uuid REFERENCES work_global_skills (id) ON DELETE CASCADE,
    version          text        NOT NULL,
    status           text        NOT NULL CHECK (status IN ('draft', 'published')),
    source_type      text        NOT NULL DEFAULT 'upload',
    name             text        NOT NULL,
    description      text        NOT NULL,
    sha256           text        NOT NULL,
    package          bytea       NOT NULL,
    files            integer     NOT NULL,
    size_bytes       bigint      NOT NULL,
    created_by       text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    CHECK ((private_skill_id IS NULL) <> (global_skill_id IS NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS work_skill_versions_private_idx
    ON work_skill_versions (private_skill_id, version) WHERE private_skill_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS work_skill_versions_global_idx
    ON work_skill_versions (global_skill_id, version) WHERE global_skill_id IS NOT NULL;

-- Existing uploads become their published version "<n>.0.0".
INSERT INTO work_skill_versions (id, private_skill_id, version, status, source_type, name, description, sha256,
                                 package, files, size_bytes, created_by, created_at)
SELECT gen_random_uuid(), s.id, s.version::text || '.0.0', 'published', 'upload', s.name, s.description, s.sha256,
       s.package, s.files, s.size_bytes, s.user_login, s.updated_at
FROM work_skills s
WHERE s.published_version_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM work_skill_versions v WHERE v.private_skill_id = s.id);

UPDATE work_skills s SET published_version_id = v.id, latest_version_id = v.id
FROM work_skill_versions v
WHERE v.private_skill_id = s.id AND s.published_version_id IS NULL AND s.latest_version_id IS NULL;

-- Results of idempotent commands (Idempotency-Key): replayed for the same
-- request, 409 for another request with the same key.
CREATE TABLE IF NOT EXISTS work_skill_commands (
    user_login      text        NOT NULL,
    command         text        NOT NULL,
    idempotency_key text        NOT NULL,
    request_hash    text        NOT NULL,
    status_code     integer     NOT NULL,
    response        text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_login, command, idempotency_key)
);

-- Guided skill creation sessions: the thread runs in mode skill_creation
-- (skill-creator attached, package written under package_root).
CREATE TABLE IF NOT EXISTS work_skill_creation_sessions (
    thread_id    uuid PRIMARY KEY REFERENCES work_threads (id) ON DELETE CASCADE,
    user_login   text        NOT NULL,
    mode         text        NOT NULL DEFAULT 'skill_creation',
    action       text        NOT NULL DEFAULT 'create',
    package_root text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS work_skill_creation_sessions_user_idx
    ON work_skill_creation_sessions (user_login, created_at DESC);
