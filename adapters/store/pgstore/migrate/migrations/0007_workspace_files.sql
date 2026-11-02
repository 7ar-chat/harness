-- Work Chat file persistence: the local Git history of /workspace (checkpoints)
-- survives sandbox expiry. After each run the sandbox's /workspace/.git is
-- archived (tar.gz) into the blob store under a per-thread key and restored
-- into every new sandbox of the thread, with the artifacts.
CREATE TABLE IF NOT EXISTS work_workspace_git (
    thread_id  uuid        PRIMARY KEY REFERENCES work_threads (id) ON DELETE CASCADE,
    blob_key   text        NOT NULL,
    head       text        NOT NULL DEFAULT '',
    size_bytes bigint      NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- The archive blobs are referenced by no artifact version: the blob garbage
-- collection (work_blob_gc) deletes them once queued. A replaced archive is
-- queued when its row is updated, the current one when its thread is purged
-- (the cascade from work_threads deletes the row).
CREATE OR REPLACE FUNCTION work_workspace_git_gc() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        INSERT INTO work_blob_gc (key, last_ref_at) VALUES (OLD.blob_key, now())
            ON CONFLICT (key) DO UPDATE SET last_ref_at = now();
        RETURN OLD;
    END IF;
    IF OLD.blob_key IS DISTINCT FROM NEW.blob_key THEN
        INSERT INTO work_blob_gc (key, last_ref_at) VALUES (OLD.blob_key, now())
            ON CONFLICT (key) DO UPDATE SET last_ref_at = now();
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS work_workspace_git_gc ON work_workspace_git;
CREATE TRIGGER work_workspace_git_gc
    AFTER UPDATE OR DELETE ON work_workspace_git
    FOR EACH ROW EXECUTE FUNCTION work_workspace_git_gc();
