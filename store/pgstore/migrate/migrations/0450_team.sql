-- Teams: users from the identity provider, projects, memberships, invites, the
-- thread-to-project mapping and an audit log. Threads stay in work_threads under
-- their owner's login; work_thread_meta says which project they belong to.
CREATE TABLE IF NOT EXISTS work_users (
    login       text PRIMARY KEY,
    email       text NOT NULL DEFAULT '',
    name        text NOT NULL DEFAULT '',
    avatar_url  text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    last_seen   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS work_users_email_idx ON work_users (email) WHERE email <> '';

CREATE TABLE IF NOT EXISTS work_projects (
    id                  text PRIMARY KEY,
    name                text NOT NULL,
    description         text NOT NULL DEFAULT '',
    repo_url            text NOT NULL DEFAULT '',
    default_branch      text NOT NULL DEFAULT '',
    github_installation bigint NOT NULL DEFAULT 0,
    created_by          text NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS work_project_members (
    project_id text NOT NULL REFERENCES work_projects(id) ON DELETE CASCADE,
    login      text NOT NULL,
    role       text NOT NULL CHECK (role IN ('owner', 'member', 'viewer')),
    added_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, login)
);
CREATE INDEX IF NOT EXISTS work_project_members_login_idx ON work_project_members (login);

CREATE TABLE IF NOT EXISTS work_project_invites (
    project_id text NOT NULL REFERENCES work_projects(id) ON DELETE CASCADE,
    email      text NOT NULL,
    role       text NOT NULL CHECK (role IN ('owner', 'member', 'viewer')),
    invited_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, email)
);

CREATE TABLE IF NOT EXISTS work_thread_meta (
    thread_id  text PRIMARY KEY,
    project_id text REFERENCES work_projects(id) ON DELETE CASCADE,
    owner      text NOT NULL,
    created_by text NOT NULL,
    kind       text NOT NULL DEFAULT 'chat',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS work_thread_meta_project_idx ON work_thread_meta (project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS work_thread_meta_owner_kind_idx ON work_thread_meta (owner, kind);

CREATE TABLE IF NOT EXISTS work_audit (
    id           bigserial PRIMARY KEY,
    project_id   text NOT NULL DEFAULT '',
    thread_id    text NOT NULL DEFAULT '',
    actor        text NOT NULL,
    on_behalf_of text NOT NULL DEFAULT '',
    action       text NOT NULL,
    detail       jsonb,
    at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS work_audit_project_idx ON work_audit (project_id, id DESC);
