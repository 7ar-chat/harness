-- Skills marketplace sync (AstraForge's marketplace_sync parity): global skills
-- imported from a Git repository (skills/<namespace>/.../SKILL.md) keep their
-- source, so a later sync updates them, and the ones removed from the
-- repository are unpublished ("deprecated").

CREATE TABLE IF NOT EXISTS work_skill_marketplace_sources (
    global_skill_id  uuid PRIMARY KEY REFERENCES work_global_skills (id) ON DELETE CASCADE,
    repository_url   text        NOT NULL,
    branch           text        NOT NULL,
    source_path      text        NOT NULL,
    namespace        text        NOT NULL DEFAULT '',
    commit_sha       text        NOT NULL DEFAULT '',
    declared_version text        NOT NULL DEFAULT '',
    deprecated       boolean     NOT NULL DEFAULT false,
    last_synced_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS work_skill_marketplace_sources_repo_idx
    ON work_skill_marketplace_sources (repository_url, branch);

-- One row per sync (AstraForge's SkillCatalogSyncRun).
CREATE TABLE IF NOT EXISTS work_skill_marketplace_runs (
    id               uuid PRIMARY KEY,
    trigger          text        NOT NULL,
    status           text        NOT NULL CHECK (status IN ('running', 'succeeded', 'partial', 'failed')),
    repository_url   text        NOT NULL,
    branch           text        NOT NULL,
    commit_sha       text        NOT NULL DEFAULT '',
    discovered_count integer     NOT NULL DEFAULT 0,
    created_count    integer     NOT NULL DEFAULT 0,
    updated_count    integer     NOT NULL DEFAULT 0,
    unchanged_count  integer     NOT NULL DEFAULT 0,
    skipped_count    integer     NOT NULL DEFAULT 0,
    deprecated_count integer     NOT NULL DEFAULT 0,
    failed_count     integer     NOT NULL DEFAULT 0,
    warnings         jsonb       NOT NULL DEFAULT '[]',
    errors           jsonb       NOT NULL DEFAULT '[]',
    started_at       timestamptz NOT NULL DEFAULT now(),
    finished_at      timestamptz
);

CREATE INDEX IF NOT EXISTS work_skill_marketplace_runs_started_idx
    ON work_skill_marketplace_runs (started_at DESC);
