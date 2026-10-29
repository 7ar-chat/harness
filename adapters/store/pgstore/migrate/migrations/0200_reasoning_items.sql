-- Stream R (Responses API, docs/harness-v3.md "R" block): the opaque reasoning
-- items (id, encrypted_content, summary, model) an assistant message was
-- generated with. They are sent back to the same model before the message's
-- own items (store:false), so its reasoning survives tool steps and runs, and
-- they go with the message: a compaction that folds the turn drops them too.
-- Never exposed by the API.
ALTER TABLE work_messages ADD COLUMN IF NOT EXISTS reasoning_items jsonb;
