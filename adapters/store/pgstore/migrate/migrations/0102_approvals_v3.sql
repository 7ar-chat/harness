-- Harness v3 §7 (approvals): "Allow for this task" (scope run) and expiry.
ALTER TABLE work_approvals ADD COLUMN IF NOT EXISTS scope text NOT NULL DEFAULT 'once';
ALTER TABLE work_approvals DROP CONSTRAINT IF EXISTS work_approvals_scope_check;
ALTER TABLE work_approvals ADD CONSTRAINT work_approvals_scope_check CHECK (scope IN ('once', 'run'));

-- Pending approvals older than APPROVAL_TTL_HOURS become expired.
ALTER TABLE work_approvals DROP CONSTRAINT IF EXISTS work_approvals_status_check;
ALTER TABLE work_approvals ADD CONSTRAINT work_approvals_status_check
    CHECK (status IN ('pending', 'resolved', 'cancelled', 'expired'));

-- Tool kinds approved with scope run, inherited along the run chain (resume runs).
ALTER TABLE work_runs ADD COLUMN IF NOT EXISTS approved_tools text[] NOT NULL DEFAULT '{}';
