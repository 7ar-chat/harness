-- Embeddings of memory entries (vectors are compared in the application: a memory holds at most a
-- few thousand entries; move to pgvector when that stops being true). A vector is stale when its
-- content_hash differs from the entry's.
CREATE TABLE IF NOT EXISTS work_memory_embeddings (
    user_login   text NOT NULL,
    entry_id     text NOT NULL,
    model        text NOT NULL,
    content_hash text NOT NULL DEFAULT '',
    vec          real[] NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_login, entry_id)
);
