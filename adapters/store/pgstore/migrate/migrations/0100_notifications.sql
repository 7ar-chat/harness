-- Harness v3 §8 (notifications): a thread is unread when one of its runs reached a
-- terminal state after the owner last saw it (POST /threads/{id}/seen).
ALTER TABLE work_threads ADD COLUMN IF NOT EXISTS last_seen_at timestamptz;
-- Existing threads count as seen (no burst of unread threads at deploy).
UPDATE work_threads SET last_seen_at = now() WHERE last_seen_at IS NULL;
ALTER TABLE work_threads ALTER COLUMN last_seen_at SET DEFAULT now();

-- lastRun / unread of the thread list.
CREATE INDEX IF NOT EXISTS work_runs_thread_created_idx ON work_runs (thread_id, created_at DESC);
CREATE INDEX IF NOT EXISTS work_runs_thread_finished_idx ON work_runs (thread_id, finished_at DESC) WHERE finished_at IS NOT NULL;
