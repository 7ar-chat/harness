-- More repositories a project works on besides repo_url (each is cloned beside the first in every thread's sandbox).
ALTER TABLE work_projects ADD COLUMN IF NOT EXISTS extra_repos text[] NOT NULL DEFAULT '{}';
