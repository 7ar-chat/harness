-- A run remembers its sandbox mode (auto | off | on); NULL is auto. A resumed
-- run inherits the mode of the thread's previous run.
ALTER TABLE work_runs ADD COLUMN IF NOT EXISTS sandbox_mode text;
ALTER TABLE work_runs DROP CONSTRAINT IF EXISTS work_runs_sandbox_mode_check;
ALTER TABLE work_runs ADD CONSTRAINT work_runs_sandbox_mode_check
    CHECK (sandbox_mode IS NULL OR sandbox_mode IN ('auto', 'off', 'on'));
