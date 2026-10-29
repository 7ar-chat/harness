-- Who wrote a message: the user (login) for user messages of shared project
-- threads, the kind (user | supervisor | system) and, for the supervisor, the
-- user it acted for. NULL for assistant and tool messages and for messages
-- written before authorship existed.
ALTER TABLE work_messages ADD COLUMN IF NOT EXISTS author_login text;
ALTER TABLE work_messages ADD COLUMN IF NOT EXISTS author_kind text;
ALTER TABLE work_messages ADD COLUMN IF NOT EXISTS on_behalf_of text;
