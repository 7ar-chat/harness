-- Approvals (interactive mode): runs remember their interaction mode, threads
-- the last mode chosen, and risky tool calls wait in work_approvals for the
-- user's decision (approve / deny / respond).

ALTER TABLE work_runs ADD COLUMN IF NOT EXISTS interaction_mode text;
ALTER TABLE work_runs DROP CONSTRAINT IF EXISTS work_runs_interaction_mode_check;
ALTER TABLE work_runs ADD CONSTRAINT work_runs_interaction_mode_check
    CHECK (interaction_mode IS NULL OR interaction_mode IN ('interactive', 'automatic'));

ALTER TABLE work_threads ADD COLUMN IF NOT EXISTS interaction_mode text;
ALTER TABLE work_threads DROP CONSTRAINT IF EXISTS work_threads_interaction_mode_check;
ALTER TABLE work_threads ADD CONSTRAINT work_threads_interaction_mode_check
    CHECK (interaction_mode IS NULL OR interaction_mode IN ('interactive', 'automatic'));

CREATE TABLE IF NOT EXISTS work_approvals (
    id                 uuid        PRIMARY KEY,
    kind               text        NOT NULL DEFAULT 'risky_tool' CHECK (kind IN ('risky_tool')),
    thread_id          uuid        NOT NULL REFERENCES work_threads (id) ON DELETE CASCADE,
    -- the run that paused before the tool call
    run_id             uuid        NOT NULL REFERENCES work_runs (id) ON DELETE CASCADE,
    tool_call_id       text        NOT NULL,
    tool_name          text        NOT NULL,
    -- canonical JSON of the call's arguments (sorted keys), matched on approval
    tool_input_preview text        NOT NULL DEFAULT '',
    status             text        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'resolved', 'cancelled')),
    decision           text        CHECK (decision IS NULL OR decision IN ('approve', 'deny', 'respond')),
    selected_choice    text,
    response_text      text,
    -- the run created by the decision (or by the message that cancelled it)
    resume_run_id      uuid,
    created_at         timestamptz NOT NULL DEFAULT now(),
    resolved_at        timestamptz
);

-- At most one pending approval per run (the run ends on it).
CREATE UNIQUE INDEX IF NOT EXISTS work_approvals_one_pending_per_run
    ON work_approvals (run_id)
    WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS work_approvals_thread_pending_idx
    ON work_approvals (thread_id, created_at)
    WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS work_approvals_resume_run_idx
    ON work_approvals (resume_run_id)
    WHERE resume_run_id IS NOT NULL;
