-- A copy that another client moved or deleted: no longer on the server,
-- but not deleted by nought (so it has no journal entry).
ALTER TABLE server_copies ADD COLUMN vanished_at INTEGER;
