-- Thread state: small JSON documents a feature keeps per conversation (the queue of messages waiting for the
-- running reply, the runtime environment of the thread, the artifacts published into answers). No foreign key:
-- the documents of a deleted thread are dropped by the harness, and a stray row is harmless.
CREATE TABLE IF NOT EXISTS work_thread_state (
    thread_id  text NOT NULL,
    key        text NOT NULL,
    doc        jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (thread_id, key)
);
