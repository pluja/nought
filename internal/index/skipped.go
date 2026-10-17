// SPDX-License-Identifier: AGPL-3.0-or-later

package index

import (
	"context"
	"fmt"
)

// SkippedMessage is a message left on the server for its size.
type SkippedMessage struct {
	UID  uint32
	Size int64
}

// RecordSkipped remembers a message left on the server for its size, so
// later syncs report it and fetch it once the limit allows.
func (ix *Index) RecordSkipped(ctx context.Context, account, folder string, uidValidity, uid uint32, size int64) error {
	_, err := ix.db.ExecContext(ctx, `
		INSERT INTO skipped (account, folder, uidvalidity, uid, size) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT DO UPDATE SET size = excluded.size`, account, folder, uidValidity, uid, size)
	if err != nil {
		return fmt.Errorf("record skipped message: %w", err)
	}
	return nil
}

// Skipped lists a folder's messages left on the server for their size, by UID.
func (ix *Index) Skipped(ctx context.Context, account, folder string, uidValidity uint32) ([]SkippedMessage, error) {
	rows, err := ix.db.QueryContext(ctx, `
		SELECT uid, size FROM skipped WHERE account = ? AND folder = ? AND uidvalidity = ? ORDER BY uid`,
		account, folder, uidValidity)
	if err != nil {
		return nil, fmt.Errorf("skipped messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SkippedMessage
	for rows.Next() {
		var s SkippedMessage
		if err := rows.Scan(&s.UID, &s.Size); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ForgetSkipped drops a skipped message: archived now, or gone from the server.
func (ix *Index) ForgetSkipped(ctx context.Context, account, folder string, uidValidity, uid uint32) error {
	_, err := ix.db.ExecContext(ctx, `
		DELETE FROM skipped WHERE account = ? AND folder = ? AND uidvalidity = ? AND uid = ?`,
		account, folder, uidValidity, uid)
	return err
}
