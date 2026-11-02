-- When each user last saw each thread: the unread dots follow the user across devices and replicas.
CREATE TABLE IF NOT EXISTS work_thread_seen (
    login     text        NOT NULL REFERENCES work_users(login) ON DELETE CASCADE,
    thread_id text        NOT NULL,
    seen_at   timestamptz NOT NULL,
    PRIMARY KEY (login, thread_id)
);
