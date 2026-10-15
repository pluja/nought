// SPDX-License-Identifier: AGPL-3.0-or-later

package index

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Errors returned by lookups.
var (
	ErrNotFound = errors.New("no such message")
)

// AmbiguousError reports that an ID prefix matches several messages.
type AmbiguousError struct {
	Prefix     string
	Candidates []string // "account/hash"
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("id %q matches %d messages: %s", e.Prefix, len(e.Candidates), strings.Join(e.Candidates, ", "))
}

// Message is the indexed metadata of one message.
type Message struct {
	ID        int64
	Account   string
	Hash      string
	MessageID string
	Date      time.Time // zero if unknown
	From      string
	To        []string
	Subject   string
	Size      int64
	ThreadKey string
	Tags      []string // sorted
}

// AddMessage indexes m with its initial tags and full-text body. If the
// account already has a message with this hash, nothing changes and created
// is false.
func (ix *Index) AddMessage(ctx context.Context, m Message, body string, now time.Time) (id int64, created bool, err error) {
	if !validHash(m.Hash) {
		return 0, false, fmt.Errorf("invalid hash %q", m.Hash)
	}
	for _, t := range m.Tags {
		if err := ValidateTag(t); err != nil {
			return 0, false, err
		}
	}
	err = ix.tx(ctx, func(tx *sql.Tx) error {
		accountID, err := ensureAccount(ctx, tx, m.Account)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO messages (account_id, hash, message_id, date, from_addr, to_addrs, subject, size, thread_key, indexed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (account_id, hash) DO NOTHING`,
			accountID, m.Hash, m.MessageID, unix(m.Date), m.From, strings.Join(m.To, "\n"),
			m.Subject, m.Size, m.ThreadKey, now.Unix())
		if err != nil {
			return fmt.Errorf("insert message: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE account_id = ? AND hash = ?`,
				accountID, m.Hash).Scan(&id)
		}
		created = true
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		if err := addTags(ctx, tx, id, m.Tags); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO messages_fts (rowid, subject, from_addr, to_addrs, body) VALUES (?, ?, ?, ?, ?)`,
			id, m.Subject, m.From, strings.Join(m.To, "\n"), body)
		if err != nil {
			return fmt.Errorf("index text: %w", err)
		}
		return nil
	})
	return id, created, err
}

func ensureAccount(ctx context.Context, tx *sql.Tx, name string) (int64, error) {
	if name == "" {
		return 0, errors.New("message has no account")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO accounts (name) VALUES (?) ON CONFLICT (name) DO NOTHING`, name); err != nil {
		return 0, fmt.Errorf("add account: %w", err)
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM accounts WHERE name = ?`, name).Scan(&id)
	return id, err
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// HasHash reports whether the account already has a message with this hash.
func (ix *Index) HasHash(ctx context.Context, account, hash string) (bool, error) {
	return ix.exists(ctx, `SELECT 1 FROM messages m JOIN accounts a ON a.id = m.account_id
		WHERE a.name = ? AND m.hash = ?`, account, hash)
}

// HasMessageID reports whether the account already has a message with this
// Message-ID. Empty IDs never match.
func (ix *Index) HasMessageID(ctx context.Context, account, messageID string) (bool, error) {
	if messageID == "" {
		return false, nil
	}
	return ix.exists(ctx, `SELECT 1 FROM messages m JOIN accounts a ON a.id = m.account_id
		WHERE a.name = ? AND m.message_id = ?`, account, messageID)
}

func (ix *Index) exists(ctx context.Context, query string, args ...any) (bool, error) {
	var one int
	err := ix.db.QueryRowContext(ctx, query+" LIMIT 1", args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Candidates returns the account's messages that have this hash or, if
// messageID is not empty, this Message-ID.
func (ix *Index) Candidates(ctx context.Context, account, hash, messageID string) ([]Message, error) {
	return ix.selectMessages(ctx, "a.name = ? AND (m.hash = ? OR (? != '' AND m.message_id = ?))",
		[]any{account, hash, messageID, messageID}, "m.id", 100)
}

// MinPrefix is the shortest accepted ID prefix.
const MinPrefix = 6

// Resolve finds the message whose hash starts with prefix, optionally
// restricted to one account.
func (ix *Index) Resolve(ctx context.Context, account, prefix string) (Message, error) {
	prefix = strings.ToLower(prefix)
	if len(prefix) < MinPrefix || len(prefix) > 64 || !isHex(prefix) {
		return Message{}, fmt.Errorf("id %q must be %d to 64 hex characters", prefix, MinPrefix)
	}
	// Hex sorts below 'g', so [prefix, prefix+"g") is exactly the prefix range
	// and can use the hash index.
	where := "m.hash >= ? AND m.hash < ?"
	args := []any{prefix, prefix + "g"}
	if account != "" {
		where += " AND a.name = ?"
		args = append(args, account)
	}
	msgs, err := ix.selectMessages(ctx, where, args, "m.id", 11)
	if err != nil {
		return Message{}, err
	}
	switch len(msgs) {
	case 0:
		return Message{}, fmt.Errorf("id %q: %w", prefix, ErrNotFound)
	case 1:
		return msgs[0], nil
	default:
		amb := &AmbiguousError{Prefix: prefix}
		for _, m := range msgs {
			amb.Candidates = append(amb.Candidates, m.Account+"/"+m.Hash[:min(len(m.Hash), 16)])
		}
		return Message{}, amb
	}
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Get returns one message by its database ID.
func (ix *Index) Get(ctx context.Context, id int64) (Message, error) {
	msgs, err := ix.selectMessages(ctx, "m.id = ?", []any{id}, "m.id", 1)
	if err != nil {
		return Message{}, err
	}
	if len(msgs) == 0 {
		return Message{}, ErrNotFound
	}
	return msgs[0], nil
}

// tagSep separates tags in group_concat; tags cannot contain control characters.
const tagSep = "\x1f"

// selectMessages loads messages. where and order must be SQL fragments
// written in this package; every user-supplied value goes in args.
func (ix *Index) selectMessages(ctx context.Context, where string, args []any, order string, limit int) ([]Message, error) {
	//nolint:gosec // G202: where/order are fixed fragments from this package, values are bound.
	query := `
		SELECT m.id, a.name, m.hash, m.message_id, m.date, m.from_addr, m.to_addrs, m.subject, m.size, m.thread_key,
		       COALESCE((SELECT group_concat(tag, char(31)) FROM (SELECT tag FROM tags WHERE message_id = m.id ORDER BY tag)), '')
		FROM messages m JOIN accounts a ON a.id = m.account_id
		WHERE ` + where + ` ORDER BY ` + order + ` LIMIT ?`
	rows, err := ix.db.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Message
	for rows.Next() {
		var m Message
		var date int64
		var to, tags string
		if err := rows.Scan(&m.ID, &m.Account, &m.Hash, &m.MessageID, &date, &m.From, &to,
			&m.Subject, &m.Size, &m.ThreadKey, &tags); err != nil {
			return nil, fmt.Errorf("read message: %w", err)
		}
		if date != 0 {
			m.Date = time.Unix(date, 0).UTC()
		}
		if to != "" {
			m.To = strings.Split(to, "\n")
		}
		m.Tags = []string{}
		if tags != "" {
			m.Tags = strings.Split(tags, tagSep)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read messages: %w", err)
	}
	return out, nil
}
