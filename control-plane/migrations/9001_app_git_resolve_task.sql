-- Additive: the most recent git-sync conflict-resolution task for the app, so
-- git/status can re-attach the console to it after a reload. Nullable.
ALTER TABLE app ADD COLUMN git_resolve_task_id TEXT;
