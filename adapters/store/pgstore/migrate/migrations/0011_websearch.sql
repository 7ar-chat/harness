-- Web search (Tavily MCP, parity with Work): the access token of a run's user,
-- encrypted with WORK_TOKEN_ENCRYPTION_KEY (AES-256-GCM, bound to the run id and
-- the login). Written by the api when the run is created (refreshed by a steer
-- carrying a token that expires later), read by the runtime for the web search
-- MCP calls, deleted when the run ends; rows of terminal runs and expired rows
-- are purged at every run end. Never shared between runs or users.
CREATE TABLE IF NOT EXISTS work_run_credentials (
    run_id     uuid PRIMARY KEY REFERENCES work_runs (id) ON DELETE CASCADE,
    user_login text        NOT NULL,
    sealed     bytea       NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS work_run_credentials_expires_idx ON work_run_credentials (expires_at);
