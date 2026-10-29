-- Blob garbage collection grace period: a queued blob is only collected once it
-- has had no reference for WORK_BLOB_GC_GRACE_HOURS (24). last_ref_at is set
-- when the last reference is dropped (thread purge) and refreshed by a new
-- artifact version referencing the blob and by an upload/snapshot about to
-- reuse the stored content, so the collection (a conditional DELETE of this
-- row) can never remove content that is being reused. Rows queued before this
-- migration start their grace period now.
ALTER TABLE work_blob_gc ADD COLUMN IF NOT EXISTS last_ref_at timestamptz NOT NULL DEFAULT now();

CREATE INDEX IF NOT EXISTS work_blob_gc_last_ref_idx ON work_blob_gc (last_ref_at);
