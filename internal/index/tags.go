// SPDX-License-Identifier: AGPL-3.0-or-later

package index

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// MaxTagLen bounds tag length.
const MaxTagLen = 128

// ValidateTag checks that a tag is non-empty, lowercase, and free of
// whitespace and control characters.
func ValidateTag(tag string) error {
	if tag == "" || len(tag) > MaxTagLen {
		return fmt.Errorf("tag %q must be 1 to %d bytes", tag, MaxTagLen)
	}
	if strings.IndexFunc(tag, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return fmt.Errorf("tag %q must not contain spaces or control characters", tag)
	}
	if tag != strings.ToLower(tag) {
		return fmt.Errorf("tag %q must be lowercase", tag)
	}
	if strings.HasPrefix(tag, "+") || strings.HasPrefix(tag, "-") {
		return fmt.Errorf("tag %q must not start with '+' or '-'", tag)
	}
	// A malformed date would sort before every real one and make the
	// message due for deletion at once.
	if d, ok := strings.CutPrefix(tag, DeleteAfterPrefix); ok {
		if _, err := time.Parse(time.DateOnly, d); err != nil || len(d) != len(time.DateOnly) {
			return fmt.Errorf("tag %q must be %sYYYY-MM-DD", tag, DeleteAfterPrefix)
		}
	}
	return nil
}

func addTags(ctx context.Context, tx *sql.Tx, id int64, tags []string) error {
	for _, t := range tags {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tags (message_id, tag) VALUES (?, ?) ON CONFLICT DO NOTHING`, id, t); err != nil {
			return fmt.Errorf("add tag: %w", err)
		}
	}
	return nil
}

// ModifyTags adds and removes tags on the given messages in one transaction.
func (ix *Index) ModifyTags(ctx context.Context, ids []int64, add, remove []string) error {
	for _, t := range slices.Concat(add, remove) {
		if err := ValidateTag(t); err != nil {
			return err
		}
	}
	return ix.tx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			for _, t := range remove {
				if _, err := tx.ExecContext(ctx, `DELETE FROM tags WHERE message_id = ? AND tag = ?`, id, t); err != nil {
					return fmt.Errorf("remove tag: %w", err)
				}
			}
			if err := addTags(ctx, tx, id, add); err != nil {
				return err
			}
		}
		return nil
	})
}

// TagCount is a tag and how many messages carry it.
type TagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// TagCounts lists every tag in use.
func (ix *Index) TagCounts(ctx context.Context) ([]TagCount, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT tag, count(*) FROM tags GROUP BY tag ORDER BY tag`)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []TagCount{}
	for rows.Next() {
		var tc TagCount
		if err := rows.Scan(&tc.Tag, &tc.Count); err != nil {
			return nil, err
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// Tag dumps use notmuch's batch-tag format, so `notmuch restore` can read
// them:
//
//	+inbox +unread -- id:abc@example.org
//
// Two extensions keep dumps lossless for nought: "# account <name>" comment
// lines scope the following lines to an account (notmuch ignores comments),
// and messages without a unique Message-ID are keyed "hash:<sha256>".
const dumpHeader = "# nought tag dump v1 (notmuch batch-tag format)"

// Dump writes the tags of every message for which keep returns true (all
// messages if keep is nil) to w, grouped by account.
func (ix *Index) Dump(ctx context.Context, w io.Writer, keep func(tags []string) bool) error {
	rows, err := ix.db.QueryContext(ctx, `
		SELECT a.name, m.hash, m.message_id,
		       CASE WHEN m.message_id = '' THEN 0 ELSE
		           (SELECT count(*) FROM messages d WHERE d.account_id = m.account_id AND d.message_id = m.message_id) END,
		       COALESCE((SELECT group_concat(tag, char(31)) FROM (SELECT tag FROM tags WHERE message_id = m.id ORDER BY tag)), '')
		FROM messages m JOIN accounts a ON a.id = m.account_id
		ORDER BY a.name, m.message_id, m.hash`)
	if err != nil {
		return fmt.Errorf("dump tags: %w", err)
	}
	defer func() { _ = rows.Close() }()

	bw := bufio.NewWriter(w)
	if _, err := fmt.Fprintln(bw, dumpHeader); err != nil {
		return err
	}
	account := ""
	for rows.Next() {
		var name, hash, msgID, tags string
		var sameID int
		if err := rows.Scan(&name, &hash, &msgID, &sameID, &tags); err != nil {
			return err
		}
		if keep != nil {
			var list []string
			if tags != "" {
				list = strings.Split(tags, tagSep)
			}
			if !keep(list) {
				continue
			}
		}
		if name != account {
			account = name
			if _, err := fmt.Fprintf(bw, "# account %s\n", name); err != nil {
				return err
			}
		}
		var line strings.Builder
		if tags != "" {
			for t := range strings.SplitSeq(tags, tagSep) {
				line.WriteString("+" + encodeTag(t) + " ")
			}
		}
		line.WriteString("-- ")
		if msgID != "" && sameID == 1 && len(msgID) <= maxDumpID && !hasControl(msgID) {
			line.WriteString("id:" + quoteID(msgID))
		} else {
			line.WriteString("hash:" + hash)
		}
		if _, err := fmt.Fprintln(bw, line.String()); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return bw.Flush()
}

// RestoreResult reports what a restore did.
type RestoreResult struct {
	Applied     int `json:"applied"`
	Missing     int `json:"missing"`
	SkippedTags int `json:"skipped_tags"` // tags nought cannot represent (e.g. with spaces)
}

// MaxDumpLine bounds one dump line. Dump never writes longer lines: long
// Message-IDs fall back to hash keys.
const MaxDumpLine = 4 << 20

// maxDumpID is the longest Message-ID written as an id: key (RFC 5322 line
// length limit).
const maxDumpID = 998

// Restore reads a dump and sets each listed message's tags exactly. Lines for
// messages not in the index are counted as missing.
func (ix *Index) Restore(ctx context.Context, r io.Reader) (RestoreResult, error) {
	return ix.restore(ctx, r, true)
}

// CheckRestore reads a dump as Restore does and reports what it would set,
// changing nothing.
func (ix *Index) CheckRestore(ctx context.Context, r io.Reader) (RestoreResult, error) {
	return ix.restore(ctx, r, false)
}

// restore applies a dump in one transaction, committed only when asked:
// a check runs every statement Restore would, then rolls back.
func (ix *Index) restore(ctx context.Context, r io.Reader, commit bool) (RestoreResult, error) {
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("begin transaction: %w", err)
	}
	res, err := restoreLines(ctx, tx, r)
	if err != nil || !commit {
		return res, errors.Join(err, tx.Rollback())
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("commit: %w", err)
	}
	return res, nil
}

func restoreLines(ctx context.Context, tx *sql.Tx, r io.Reader) (RestoreResult, error) {
	var res RestoreResult
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), MaxDumpLine)
	account := ""
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if name, ok := strings.CutPrefix(line, "# account "); ok {
			account = strings.TrimSpace(name)
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		tags, invalid, key, err := parseDumpLine(line)
		if err != nil {
			return res, fmt.Errorf("line %d: %w", lineNo, err)
		}
		res.SkippedTags += len(invalid)
		n, err := setTags(ctx, tx, account, key, tags)
		if err != nil {
			return res, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if n == 0 {
			res.Missing++
		}
		res.Applied += n
	}
	return res, sc.Err()
}

type dumpKey struct {
	field string // "id" or "hash"
	value string
}

// parseDumpLine splits "+tag ... -- key". The separator is the first
// whitespace-delimited "--" token; tags always start with "+", so a tag can
// never be mistaken for it. Tags that are not valid nought tags are returned
// separately so restore can skip them instead of failing.
func parseDumpLine(line string) (tags, invalid []string, key dumpKey, err error) {
	sep := separatorIndex(line)
	if sep < 0 {
		return nil, nil, dumpKey{}, errors.New(`missing "--" separator`)
	}
	for f := range strings.FieldsSeq(line[:sep]) {
		enc, ok := strings.CutPrefix(f, "+")
		if !ok {
			return nil, nil, dumpKey{}, fmt.Errorf("tag operation %q is not supported", f)
		}
		tag, err := decodeTag(enc)
		if err != nil {
			return nil, nil, dumpKey{}, err
		}
		tag = strings.ToLower(tag)
		if ValidateTag(tag) != nil {
			invalid = append(invalid, tag)
			continue
		}
		tags = append(tags, tag)
	}
	right := strings.TrimSpace(line[sep+2:])
	switch {
	case strings.HasPrefix(right, "id:"):
		id, err := unquoteID(right[3:])
		return tags, invalid, dumpKey{"id", id}, err
	case strings.HasPrefix(right, "hash:"):
		h := right[5:]
		if !validHash(h) {
			return nil, nil, dumpKey{}, fmt.Errorf("bad hash %q", h)
		}
		return tags, invalid, dumpKey{"hash", h}, nil
	default:
		return nil, nil, dumpKey{}, fmt.Errorf("unsupported query %q", right)
	}
}

// separatorIndex finds the first "--" that is a whole whitespace-separated token.
func separatorIndex(line string) int {
	for i := 0; i+1 < len(line); i++ {
		if line[i] != '-' || line[i+1] != '-' {
			continue
		}
		before := i == 0 || line[i-1] == ' ' || line[i-1] == '\t'
		after := i+2 == len(line) || line[i+2] == ' ' || line[i+2] == '\t'
		if before && after {
			return i
		}
	}
	return -1
}

// setTags replaces the tags of every message matching key and returns how
// many messages matched.
func setTags(ctx context.Context, tx *sql.Tx, account string, key dumpKey, tags []string) (int, error) {
	col := "m.message_id"
	if key.field == "hash" {
		col = "m.hash"
	}
	q := `SELECT m.id FROM messages m JOIN accounts a ON a.id = m.account_id WHERE ` + col + ` = ?`
	args := []any{key.value}
	if account != "" {
		q += " AND a.name = ?"
		args = append(args, account)
	}
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM tags WHERE message_id = ?`, id); err != nil {
			return 0, err
		}
		if err := addTags(ctx, tx, id, tags); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// encodeTag percent-encodes bytes outside a conservative safe set, as
// notmuch's hex encoding does; notmuch restore decodes it.
func encodeTag(tag string) string {
	var b strings.Builder
	for i := range len(tag) {
		c := tag[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-_.+@=:,/", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02x", c)
		}
	}
	return b.String()
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func decodeTag(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) || !isHexDigit(s[i+1]) || !isHexDigit(s[i+2]) {
			return "", fmt.Errorf("bad escape in tag %q", s)
		}
		c, _ := strconv.ParseUint(s[i+1:i+3], 16, 8)
		b.WriteByte(byte(c))
		i += 2
	}
	return b.String(), nil
}

// quoteID quotes a Message-ID the way notmuch quotes boolean terms: IDs with
// whitespace, quotes or parentheses are wrapped in double quotes, with
// embedded quotes doubled.
func quoteID(id string) string {
	if !strings.ContainsAny(id, " \t\"()") {
		return id
	}
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}

func unquoteID(s string) (string, error) {
	if !strings.HasPrefix(s, `"`) {
		if s == "" {
			return "", errors.New("empty id")
		}
		return s, nil
	}
	if len(s) < 2 || !strings.HasSuffix(s, `"`) {
		return "", fmt.Errorf("unterminated quoted id %q", s)
	}
	id := strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	if id == "" {
		return "", errors.New("empty id")
	}
	return id, nil
}

func hasControl(s string) bool { return strings.IndexFunc(s, unicode.IsControl) >= 0 }

func validHash(h string) bool { return len(h) == 64 && isHex(h) }
