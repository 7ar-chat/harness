-- The harness no longer carries an identity token per run: a run belongs to a plain user login.
ALTER TABLE work_runs DROP COLUMN IF EXISTS userinfo;
