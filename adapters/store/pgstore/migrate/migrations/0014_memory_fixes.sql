-- Memory fixes on top of 0010 (README "Memory").
-- version: bumped by every update; an update applies only to the version it
-- read (optimistic concurrency: the consolidation job, the agent and the user
-- cannot overwrite each other's change or resurrect a deleted entry).
ALTER TABLE work_memory_entries ADD COLUMN IF NOT EXISTS version bigint NOT NULL DEFAULT 0;

-- proposal: a change the agent made to an active entry after untrusted content
-- (memory_update). The entry keeps its confirmed content (still used) until the
-- user keeps the proposal (applied then) or discards it (dropped).
ALTER TABLE work_memory_entries ADD COLUMN IF NOT EXISTS proposal jsonb;

-- The facts 0010 migrated from work_memories kept their raw text (no secret
-- redaction) and got a 120-character first line with its markdown as title
-- (DeriveTitle strips it and keeps 200), which later edits took for a custom
-- title. They are flagged here for the Go backfill (memory.BackfillLegacy, run
-- by `work migrate` right after the migrations and retried by the consolidation
-- job) that redacts them with the memory write policy and derives their title
-- like any entry; it removes the flag.
UPDATE work_memory_entries SET metadata = metadata || '{"legacy_backfill": true}'::jsonb
WHERE source = 'legacy' AND metadata->>'migrated_from' = 'work_memories' AND NOT metadata ? 'legacy_backfill';
