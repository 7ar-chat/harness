-- The Slack channel (id) a project's team-visible notifications go to; empty means direct messages only.
ALTER TABLE work_projects ADD COLUMN IF NOT EXISTS slack_channel text NOT NULL DEFAULT '';
