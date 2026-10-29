-- A run remembers its mode (build | plan); NULL is build. A plan run is read-only
-- (the engine offers it no tool that changes anything). A resumed run inherits
-- the mode of the thread's previous run.
ALTER TABLE work_runs ADD COLUMN IF NOT EXISTS run_mode text;
ALTER TABLE work_runs DROP CONSTRAINT IF EXISTS work_runs_run_mode_check;
ALTER TABLE work_runs ADD CONSTRAINT work_runs_run_mode_check
    CHECK (run_mode IS NULL OR run_mode IN ('build', 'plan'));
