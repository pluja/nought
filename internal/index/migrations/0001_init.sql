-- Accounts are referenced by name from the config file, which owns their
-- other settings (label, color, servers).
CREATE TABLE accounts (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
);

CREATE TABLE messages (
    id         INTEGER PRIMARY KEY,
    account_id INTEGER NOT NULL REFERENCES accounts(id),
    hash       TEXT    NOT NULL,             -- SHA-256 of the raw message; names the file
    message_id TEXT    NOT NULL DEFAULT '',
    date       INTEGER NOT NULL DEFAULT 0,   -- Unix seconds, UTC; 0 if unknown
    from_addr  TEXT    NOT NULL DEFAULT '',
    to_addrs   TEXT    NOT NULL DEFAULT '',  -- newline-separated To and Cc
    subject    TEXT    NOT NULL DEFAULT '',
    size       INTEGER NOT NULL,
    thread_key TEXT    NOT NULL DEFAULT '',
    indexed_at INTEGER NOT NULL,
    UNIQUE (account_id, hash)
);
CREATE INDEX messages_hash       ON messages (hash);
CREATE INDEX messages_message_id ON messages (account_id, message_id);
CREATE INDEX messages_date       ON messages (date);
CREATE INDEX messages_thread     ON messages (thread_key);

CREATE TABLE tags (
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    tag        TEXT    NOT NULL,
    PRIMARY KEY (message_id, tag)
) WITHOUT ROWID;
CREATE INDEX tags_tag ON tags (tag);

-- rowid = messages.id
CREATE VIRTUAL TABLE messages_fts USING fts5 (
    subject, from_addr, to_addrs, body,
    tokenize = 'unicode61 remove_diacritics 2'
);
