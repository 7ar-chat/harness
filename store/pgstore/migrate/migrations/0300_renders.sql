-- Review round 1 (harness v3, P2 #2): view_file renders. The page images live in
-- the sandbox under /workspace/.work/renders/{render_id}/p{N}.png; this row is
-- their source and its confidentiality label, so GET /threads/{id}/renders/…
-- only serves pages of a source whose label allows derived files.
CREATE TABLE IF NOT EXISTS work_renders (
    thread_id   uuid        NOT NULL REFERENCES work_threads (id) ON DELETE CASCADE,
    render_id   text        NOT NULL,
    source_path text        NOT NULL,
    label       text        NOT NULL DEFAULT '',
    allowed     boolean     NOT NULL,
    pages       integer[]   NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (thread_id, render_id)
);
