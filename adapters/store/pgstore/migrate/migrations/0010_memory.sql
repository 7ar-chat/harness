-- Memory parity with Work (AstraForge memory proxy): per-user memory entries
-- with a path (shared/… or memories/semantic/{decisions,facts,constraints,
-- preferences,procedures,notes}/…), title, markdown content, tags, importance,
-- metadata (trigger…), status (active / pending / deleted) and timestamps.
-- Soft-deleted entries keep their row (status 'deleted', deleted_at) until the
-- consolidation job purges them after WORK_MEMORY_DELETED_RETENTION_DAYS.
CREATE TABLE IF NOT EXISTS work_memory_entries (
    id                 text             PRIMARY KEY,
    user_login         text             NOT NULL,
    path               text             NOT NULL CHECK (char_length(path) BETWEEN 1 AND 512),
    title              text             NOT NULL DEFAULT '',
    content            text             NOT NULL,
    tags               jsonb            NOT NULL DEFAULT '[]',
    importance         double precision CHECK (importance IS NULL OR importance BETWEEN 0 AND 1),
    metadata           jsonb            NOT NULL DEFAULT '{}',
    scope              text             NOT NULL DEFAULT 'personal',
    repository         text             NOT NULL DEFAULT 'default',
    source             text             NOT NULL DEFAULT 'proxy',
    status             text             NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'pending', 'deleted')),
    source_thread_id   uuid,
    content_hash       text             NOT NULL DEFAULT '',
    created_at         timestamptz      NOT NULL DEFAULT now(),
    updated_at         timestamptz      NOT NULL DEFAULT now(),
    content_updated_at timestamptz      NOT NULL DEFAULT now(),
    deleted_at         timestamptz
);

-- One live entry per path and user (soft-deleted ones may share it).
CREATE UNIQUE INDEX IF NOT EXISTS work_memory_entries_path_uidx ON work_memory_entries (user_login, path) WHERE status <> 'deleted';
CREATE INDEX IF NOT EXISTS work_memory_entries_user_idx ON work_memory_entries (user_login, created_at);
CREATE INDEX IF NOT EXISTS work_memory_entries_deleted_idx ON work_memory_entries (deleted_at) WHERE status = 'deleted';

-- The former per-user facts become semantic facts. Their id is derived from the
-- old uuid (mem_<32 hex>) so links from older conversations still resolve
-- (store.LegacyMemoryID); pending proposals stay pending.
INSERT INTO work_memory_entries (id, user_login, path, title, content, tags, metadata, scope, repository, source, status,
                                 source_thread_id, content_hash, created_at, updated_at, content_updated_at)
SELECT 'mem_' || replace(m.id::text, '-', ''),
       m.user_login,
       'memories/semantic/facts/' || to_char(m.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD') || '-mem_' || replace(m.id::text, '-', '') || '.md',
       left(btrim(regexp_replace(split_part(m.text, E'\n', 1), '\s+', ' ', 'g')), 120),
       m.text,
       '["fact"]'::jsonb,
       jsonb_build_object('trigger', 'explicit_remember', 'scope', 'personal', 'repository', 'default',
                          'migrated_from', 'work_memories', 'legacy_id', m.id::text),
       'personal',
       'default',
       'legacy',
       m.status,
       m.source_thread_id,
       encode(sha256(convert_to(m.text, 'UTF8')), 'hex'),
       m.created_at,
       m.created_at,
       m.created_at
FROM work_memories m
ON CONFLICT (id) DO NOTHING;

-- Everything was copied above: empty the old table so it does not keep facts
-- the user later deletes. It is kept (not dropped) so pods of the previous
-- release see an empty memory instead of failing during a rolling deploy or
-- after a rollback; drop it in a later release.
DELETE FROM work_memories;
