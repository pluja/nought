// SPDX-License-Identifier: AGPL-3.0-or-later

package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SyncState is where the last sync of a server folder stopped.
type SyncState struct {
	UIDValidity uint32
	LastUID     uint32
}

// SyncState returns a folder's state; found is false for a new folder.
func (ix *Index) SyncState(ctx context.Context, account, folder string) (st SyncState, found bool, err error) {
	err = ix.db.QueryRowContext(ctx, `SELECT uidvalidity, last_uid FROM sync_state WHERE account = ? AND folder = ?`,
		account, folder).Scan(&st.UIDValidity, &st.LastUID)
	if errors.Is(err, sql.ErrNoRows) {
		return SyncState{}, false, nil
	}
	return st, err == nil, err
}

// SetSyncState records a folder's progress.
func (ix *Index) SetSyncState(ctx context.Context, account, folder string, st SyncState) error {
	_, err := ix.db.ExecContext(ctx, `
		INSERT INTO sync_state (account, folder, uidvalidity, last_uid) VALUES (?, ?, ?, ?)
		ON CONFLICT (account, folder) DO UPDATE SET uidvalidity = excluded.uidvalidity, last_uid = excluded.last_uid`,
		account, folder, st.UIDValidity, st.LastUID)
	if err != nil {
		return fmt.Errorf("save sync state: %w", err)
	}
	return nil
}

// ResetFolder starts a folder over under a new UIDVALIDITY. UIDs from the old
// validity mean nothing any more, so their pending copies are forgotten and
// can never be deleted by UID; the folder is fetched again from the start
// and each message is matched to the archive by its content hash.
func (ix *Index) ResetFolder(ctx context.Context, account, folder string, uidValidity uint32) error {
	return ix.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM server_copies WHERE account = ? AND folder = ? AND uidvalidity != ? AND expunged_at IS NULL`,
			account, folder, uidValidity); err != nil {
			return fmt.Errorf("forget stale copies: %w", err)
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO sync_state (account, folder, uidvalidity, last_uid) VALUES (?, ?, ?, 0)
			ON CONFLICT (account, folder) DO UPDATE SET uidvalidity = excluded.uidvalidity, last_uid = 0`,
			account, folder, uidValidity)
		return err
	})
}

// ServerCopy is a message on a server whose local copy was verified.
type ServerCopy struct {
	Account      string
	Folder       string
	UIDValidity  uint32
	UID          uint32
	Hash         string
	InternalDate time.Time
}

// RecordCopy notes a server message after its local copy was verified.
// Recording the same UID again is a no-op.
func (ix *Index) RecordCopy(ctx context.Context, c ServerCopy, now time.Time) error {
	if !validHash(c.Hash) {
		return fmt.Errorf("invalid hash %q", c.Hash)
	}
	_, err := ix.db.ExecContext(ctx, `
		INSERT INTO server_copies (account, folder, uidvalidity, uid, hash, internal_date, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		c.Account, c.Folder, c.UIDValidity, c.UID, c.Hash, c.InternalDate.Unix(), now.Unix())
	if err != nil {
		return fmt.Errorf("record server copy: %w", err)
	}
	return nil
}

// DueCopies lists a folder's copies still on the server that the server
// received at or before cutoff.
func (ix *Index) DueCopies(ctx context.Context, account, folder string, uidValidity uint32, cutoff time.Time) ([]ServerCopy, error) {
	rows, err := ix.db.QueryContext(ctx, `
		SELECT uid, hash, internal_date FROM server_copies
		WHERE account = ? AND folder = ? AND uidvalidity = ? AND expunged_at IS NULL AND internal_date <= ?
		ORDER BY uid`, account, folder, uidValidity, cutoff.Unix())
	if err != nil {
		return nil, fmt.Errorf("list due copies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ServerCopy
	for rows.Next() {
		c := ServerCopy{Account: account, Folder: folder, UIDValidity: uidValidity}
		var date int64
		if err := rows.Scan(&c.UID, &c.Hash, &date); err != nil {
			return nil, err
		}
		c.InternalDate = time.Unix(date, 0).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkExpunged records that copies were deleted from the server.
func (ix *Index) MarkExpunged(ctx context.Context, copies []ServerCopy, at time.Time) error {
	return ix.tx(ctx, func(tx *sql.Tx) error {
		for _, c := range copies {
			if _, err := tx.ExecContext(ctx, `
				UPDATE server_copies SET expunged_at = ?
				WHERE account = ? AND folder = ? AND uidvalidity = ? AND uid = ?`,
				at.Unix(), c.Account, c.Folder, c.UIDValidity, c.UID); err != nil {
				return fmt.Errorf("mark expunged: %w", err)
			}
		}
		return nil
	})
}
