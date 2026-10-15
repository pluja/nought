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
	// Synced reports that the folder completed a sync since it was last
	// (re)started, so new UIDs are new arrivals or moves, not a re-fetch.
	Synced bool
}

// SyncState returns a folder's state; found is false for a new folder.
func (ix *Index) SyncState(ctx context.Context, account, folder string) (st SyncState, found bool, err error) {
	err = ix.db.QueryRowContext(ctx, `SELECT uidvalidity, last_uid, synced_at > 0 FROM sync_state WHERE account = ? AND folder = ?`,
		account, folder).Scan(&st.UIDValidity, &st.LastUID, &st.Synced)
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
		return restartFolder(ctx, tx, account, folder, uidValidity)
	})
}

// ForgetFolder drops a folder's pending copies and starts it over from the
// first UID, keeping the record of expunged ones. Used when the server broke
// UID semantics: everything is fetched and matched to the archive again.
func (ix *Index) ForgetFolder(ctx context.Context, account, folder string, uidValidity uint32) error {
	return ix.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM server_copies WHERE account = ? AND folder = ? AND expunged_at IS NULL`, account, folder); err != nil {
			return err
		}
		return restartFolder(ctx, tx, account, folder, uidValidity)
	})
}

// restartFolder makes the next sync fetch a folder from its first message,
// as a re-fetch (not new arrivals).
func restartFolder(ctx context.Context, tx *sql.Tx, account, folder string, uidValidity uint32) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO sync_state (account, folder, uidvalidity, last_uid) VALUES (?, ?, ?, 0)
		ON CONFLICT (account, folder) DO UPDATE SET uidvalidity = excluded.uidvalidity, last_uid = 0, synced_at = 0`,
		account, folder, uidValidity)
	return err
}

// MarkSynced records that a folder finished syncing at t.
func (ix *Index) MarkSynced(ctx context.Context, account, folder string, t time.Time) error {
	_, err := ix.db.ExecContext(ctx, `UPDATE sync_state SET synced_at = ? WHERE account = ? AND folder = ?`,
		t.Unix(), account, folder)
	return err
}
