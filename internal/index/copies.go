// SPDX-License-Identifier: AGPL-3.0-or-later

package index

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ServerCopy is a message on a server whose local copy was verified.
type ServerCopy struct {
	Account      string
	Folder       string // the server's raw folder name
	FolderName   string // for display ("Archive/2024" for "Archive.2024")
	UIDValidity  uint32
	UID          uint32
	Hash         string
	Size         int64
	InternalDate time.Time
}

// ErrCopyConflict means a server UID was seen before with different
// content: the server broke UID semantics (e.g. after a restore), and
// nothing in that folder may be deleted by UID.
var ErrCopyConflict = errors.New("server reused a UID for different content")

// RecordCopy notes a server message after its local copy was verified.
// Recording the same UID with the same content again is a no-op; with
// different content it is ErrCopyConflict. A missing (zero) INTERNALDATE is
// recorded as the fetch time, so keep_days and max_days count from
// the fetch.
func (ix *Index) RecordCopy(ctx context.Context, c ServerCopy, now time.Time) error {
	if !validHash(c.Hash) {
		return fmt.Errorf("invalid hash %q", c.Hash)
	}
	date := c.InternalDate
	if date.IsZero() || date.Unix() <= 0 {
		date = now
	}
	return ix.tx(ctx, func(tx *sql.Tx) error {
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT hash FROM server_copies
			WHERE account = ? AND folder = ? AND uidvalidity = ? AND uid = ?`,
			c.Account, c.Folder, c.UIDValidity, c.UID).Scan(&existing)
		switch {
		case err == nil && existing == c.Hash:
			return nil
		case err == nil:
			return fmt.Errorf("uid %d: %w", c.UID, ErrCopyConflict)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO server_copies (account, folder, folder_name, uidvalidity, uid, hash, size, internal_date, fetched_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.Account, c.Folder, cmp.Or(c.FolderName, c.Folder), c.UIDValidity, c.UID, c.Hash, c.Size, date.Unix(), now.Unix())
		if err != nil {
			return fmt.Errorf("record server copy: %w", err)
		}
		return nil
	})
}

// live is the SQL condition for a copy still on the server: neither
// deleted by nought nor moved or deleted by another client.
const live = `expunged_at IS NULL AND vanished_at IS NULL`

// ExpiredCopies lists a folder's copies still on the server that the server
// received at or before cutoff (for max_days, whatever the inbox says).
func (ix *Index) ExpiredCopies(ctx context.Context, account, folder string, uidValidity uint32, cutoff time.Time) ([]ServerCopy, error) {
	return ix.copies(ctx, `account = ? AND folder = ? AND uidvalidity = ? AND `+live+` AND internal_date <= ?`,
		account, folder, uidValidity, cutoff.Unix())
}

// LiveCopies lists a folder's copies still on the server.
func (ix *Index) LiveCopies(ctx context.Context, account, folder string, uidValidity uint32) ([]ServerCopy, error) {
	return ix.copies(ctx, `account = ? AND folder = ? AND uidvalidity = ? AND `+live, account, folder, uidValidity)
}

// HandledCopies lists a folder's copies still on the server, received at or
// before cutoff, whose message is no longer in the inbox (archived or
// deleted, here or on another client).
func (ix *Index) HandledCopies(ctx context.Context, account, folder string, uidValidity uint32, cutoff time.Time) ([]ServerCopy, error) {
	return ix.copies(ctx, `account = ? AND folder = ? AND uidvalidity = ? AND `+live+` AND internal_date <= ?
		AND NOT EXISTS (SELECT 1 FROM messages m JOIN accounts a ON a.id = m.account_id JOIN tags t ON t.message_id = m.id
		                WHERE a.name = server_copies.account AND m.hash = server_copies.hash AND t.tag = 'inbox')`,
		account, folder, uidValidity, cutoff.Unix())
}

// copies lists the server copies matching where, in UID order.
func (ix *Index) copies(ctx context.Context, where string, args ...any) ([]ServerCopy, error) {
	//nolint:gosec // G202: callers pass constant conditions; values are "?" arguments.
	rows, err := ix.db.QueryContext(ctx, `
		SELECT account, folder, folder_name, uidvalidity, uid, hash, size, internal_date FROM server_copies
		WHERE `+where+` ORDER BY uid`, args...)
	if err != nil {
		return nil, fmt.Errorf("list server copies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ServerCopy
	for rows.Next() {
		var c ServerCopy
		var date int64
		if err := rows.Scan(&c.Account, &c.Folder, &c.FolderName, &c.UIDValidity, &c.UID, &c.Hash, &c.Size, &date); err != nil {
			return nil, err
		}
		c.InternalDate = time.Unix(date, 0).UTC()
		c.FolderName = cmp.Or(c.FolderName, c.Folder)
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkVanished records copies that another client moved or deleted.
func (ix *Index) MarkVanished(ctx context.Context, copies []ServerCopy, at time.Time) error {
	return ix.tx(ctx, func(tx *sql.Tx) error {
		for _, c := range copies {
			if _, err := tx.ExecContext(ctx, `UPDATE server_copies SET vanished_at = ?
				WHERE account = ? AND folder = ? AND uidvalidity = ? AND uid = ?`,
				at.Unix(), c.Account, c.Folder, c.UIDValidity, c.UID); err != nil {
				return fmt.Errorf("mark vanished: %w", err)
			}
		}
		return nil
	})
}

// TrackedFolders returns the account's folders that have copies still on
// the server, with the UIDVALIDITY those copies were recorded under.
func (ix *Index) TrackedFolders(ctx context.Context, account string) (map[string]uint32, error) {
	rows, err := ix.db.QueryContext(ctx, `
		SELECT DISTINCT c.folder, c.uidvalidity FROM server_copies c JOIN sync_state s
		  ON s.account = c.account AND s.folder = c.folder AND s.uidvalidity = c.uidvalidity
		WHERE c.account = ? AND `+live, account)
	if err != nil {
		return nil, fmt.Errorf("tracked folders: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]uint32{}
	for rows.Next() {
		var folder string
		var validity uint32
		if err := rows.Scan(&folder, &validity); err != nil {
			return nil, err
		}
		out[folder] = validity
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

// MessageIDOf returns the Message-ID of an account's message by hash, or ""
// (for the expunge journal).
func (ix *Index) MessageIDOf(ctx context.Context, account, hash string) string {
	var id string
	_ = ix.db.QueryRowContext(ctx, `SELECT m.message_id FROM messages m JOIN accounts a ON a.id = m.account_id
		WHERE a.name = ? AND m.hash = ?`, account, hash).Scan(&id)
	return id
}

// OnServer returns, for each of an account's hashes, the copies still on
// the server (not expunged).
func (ix *Index) OnServer(ctx context.Context, account string, hashes []string) (map[string][]ServerCopy, error) {
	out := make(map[string][]ServerCopy)
	for chunk := range slices.Chunk(hashes, 500) {
		args := []any{account}
		marks := make([]string, len(chunk))
		for i, h := range chunk {
			marks[i] = "?"
			args = append(args, h)
		}
		//nolint:gosec // G202: only "?" placeholders are concatenated.
		rows, err := ix.db.QueryContext(ctx, `
			SELECT folder, folder_name, uidvalidity, uid, hash, size, internal_date FROM server_copies
			WHERE account = ? AND `+live+` AND hash IN (`+strings.Join(marks, ",")+`)
			  AND uidvalidity = (SELECT s.uidvalidity FROM sync_state s
			                     WHERE s.account = server_copies.account AND s.folder = server_copies.folder)
			ORDER BY folder_name`, args...)
		if err != nil {
			return nil, fmt.Errorf("server copies: %w", err)
		}
		for rows.Next() {
			c := ServerCopy{Account: account}
			var date int64
			if err := rows.Scan(&c.Folder, &c.FolderName, &c.UIDValidity, &c.UID, &c.Hash, &c.Size, &date); err != nil {
				return nil, errors.Join(err, rows.Close())
			}
			c.InternalDate = time.Unix(date, 0).UTC()
			c.FolderName = cmp.Or(c.FolderName, c.Folder)
			out[c.Hash] = append(out[c.Hash], c)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// AccountServerStatus summarizes an account: how many archived messages
// are still on its server, and when all of its folders were last synced
// (the oldest folder's time, so one successful folder does not hide others).
type AccountServerStatus struct {
	OnServer   int
	LastSynced time.Time // zero if never
}

// ServerStatus returns the status of an account.
func (ix *Index) ServerStatus(ctx context.Context, account string) (AccountServerStatus, error) {
	var st AccountServerStatus
	var last int64
	err := ix.db.QueryRowContext(ctx, `
		SELECT (SELECT count(DISTINCT c.hash) FROM server_copies c JOIN sync_state s
		          ON s.account = c.account AND s.folder = c.folder AND s.uidvalidity = c.uidvalidity
		        WHERE c.account = ? AND `+live+`),
		       COALESCE((SELECT min(synced_at) FROM sync_state WHERE account = ? AND synced_at > 0), 0)`,
		account, account).Scan(&st.OnServer, &last)
	if err != nil {
		return st, fmt.Errorf("server status: %w", err)
	}
	if last > 0 {
		st.LastSynced = time.Unix(last, 0).UTC()
	}
	return st, nil
}

// DeletionRequested lists a folder's copies still on the server whose
// message the user deleted (tag "deleted") or whose delete-after date has
// come: sync removes these regardless of the folder's keep_days and the
// account's automatic deletion setting.
func (ix *Index) DeletionRequested(ctx context.Context, account, folder string, uidValidity uint32, today string) ([]ServerCopy, error) {
	return ix.copies(ctx, `account = ? AND folder = ? AND uidvalidity = ? AND `+live+`
		AND EXISTS (SELECT 1 FROM messages m JOIN accounts a ON a.id = m.account_id JOIN tags t ON t.message_id = m.id
		            WHERE a.name = server_copies.account AND m.hash = server_copies.hash
		              AND (t.tag = 'deleted' OR `+dueTag+`))`, account, folder, uidValidity, today)
}
