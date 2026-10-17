-- Messages left on the server for their size: reported at every sync while
-- they are there, and fetched once the size limit allows.
CREATE TABLE skipped (
    account     TEXT    NOT NULL,
    folder      TEXT    NOT NULL,
    uidvalidity INTEGER NOT NULL,
    uid         INTEGER NOT NULL,
    size        INTEGER NOT NULL,  -- RFC822.SIZE when last seen
    PRIMARY KEY (account, folder, uidvalidity, uid)
) WITHOUT ROWID;
