-- When each folder was last synced, and the display name of the folder each
-- server copy is in, so frontends can show where a message lives.
ALTER TABLE sync_state ADD COLUMN synced_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE server_copies ADD COLUMN folder_name TEXT NOT NULL DEFAULT '';
CREATE INDEX server_copies_hash ON server_copies (account, hash, expunged_at);
