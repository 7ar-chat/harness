-- Sign-up approval: an account created through "Continue with GitHub" waits as 'pending' until an admin approves it.
-- Every account that exists before this migration was let in, so it is 'approved'.
ALTER TABLE work_users ADD COLUMN IF NOT EXISTS status text NOT NULL DEFAULT 'approved';
