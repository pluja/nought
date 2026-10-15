-- Per server folder: where the last sync stopped.
CREATE TABLE sync_state (
    account     TEXT    NOT NULL,
    folder      TEXT    NOT NULL,
    uidvalidity INTEGER NOT NULL,
    last_uid    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (account, folder)
) WITHOUT ROWID;

-- Every message seen on a server, with the hash of its verified local copy.
-- A row is only written after the local copy is verified, so these are the
-- only messages sync may ever delete from the server.
CREATE TABLE server_copies (
    account       TEXT    NOT NULL,
    folder        TEXT    NOT NULL,
    uidvalidity   INTEGER NOT NULL,
    uid           INTEGER NOT NULL,
    hash          TEXT    NOT NULL,
    size          INTEGER NOT NULL,  -- RFC822.SIZE, re-checked before deletion
    internal_date INTEGER NOT NULL,  -- Unix seconds: when the server received it
    fetched_at    INTEGER NOT NULL,
    expunged_at   INTEGER,           -- NULL while still on the server
    PRIMARY KEY (account, folder, uidvalidity, uid)
) WITHOUT ROWID;
CREATE INDEX server_copies_due ON server_copies (account, folder, uidvalidity, expunged_at, internal_date);
