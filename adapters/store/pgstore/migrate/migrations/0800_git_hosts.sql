-- Code hosts besides GitHub.com (GitLab, Gitea/Forgejo, plain git) that an administrator allows. The host document is
-- jsonb (kind, URLs, SSH host keys); token is the sealed shared service token, if one was set. A user's own access
-- token or SSH key is a connector (work_connectors, kind git:<host> and gitssh:<host>), sealed by the server.
CREATE TABLE IF NOT EXISTS work_git_hosts (
    name       text PRIMARY KEY,
    host       jsonb NOT NULL,
    token      bytea,
    updated_at timestamptz NOT NULL
);
