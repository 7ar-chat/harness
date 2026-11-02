-- RUN_ERROR code of failed/interrupted runs (used to synthesize a terminal event on replay).
ALTER TABLE work_runs ADD COLUMN IF NOT EXISTS error_code text;
