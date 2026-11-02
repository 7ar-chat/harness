-- Harness v3 §9: the thread list is keyset-paged on the immutable created_at.
CREATE INDEX IF NOT EXISTS work_threads_user_created_idx
    ON work_threads (user_login, (pinned_at IS NOT NULL) DESC, created_at DESC, id DESC)
    WHERE deleted_at IS NULL;
