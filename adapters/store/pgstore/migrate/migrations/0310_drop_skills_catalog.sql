-- Skills are filesystem-based now (built-in, user directory): the database
-- catalog of uploaded skill versions, global skills, per-user toggles,
-- idempotent skill commands, guided creation sessions and the marketplace
-- sync is gone. work_runs.skills (the names requested for a run) stays.
DROP TABLE IF EXISTS work_skill_marketplace_runs;
DROP TABLE IF EXISTS work_skill_marketplace_sources;
DROP TABLE IF EXISTS work_skill_creation_sessions;
DROP TABLE IF EXISTS work_skill_commands;
DROP TABLE IF EXISTS work_skill_versions;
DROP TABLE IF EXISTS work_global_skills;
DROP TABLE IF EXISTS work_skill_settings;
DROP TABLE IF EXISTS work_skills;
DROP TABLE IF EXISTS work_run_credentials;
