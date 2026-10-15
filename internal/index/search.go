// SPDX-License-Identifier: AGPL-3.0-or-later

package index

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"nought/internal/query"
)

// MaxLimit caps how many messages one search returns.
const MaxLimit = 10000

// Search returns messages matching q, newest first. Up to MaxLimit+1 may be
// requested, so callers can tell whether more than MaxLimit exist.
func (ix *Index) Search(ctx context.Context, q query.Query, limit, offset int) ([]Message, error) {
	if limit <= 0 || limit > MaxLimit+1 {
		limit = MaxLimit
	}
	where, args, err := compile(q)
	if err != nil {
		return nil, err
	}
	if offset < 0 {
		offset = 0
	}
	// Page in a subquery, then load the page with its tags.
	page := "m.id IN (SELECT m.id FROM messages m JOIN accounts a ON a.id = m.account_id WHERE " + where +
		" ORDER BY m.date DESC, m.id DESC LIMIT ? OFFSET ?)"
	return ix.selectMessages(ctx, page, append(args, limit, offset), "m.date DESC, m.id DESC", limit)
}

// compile turns a query into a WHERE clause over "messages m JOIN accounts a".
// Every user-supplied value is a bound parameter; only fixed SQL fragments
// are concatenated.
func compile(q query.Query) (string, []any, error) {
	if len(q.Groups) == 0 {
		return "1", nil, nil
	}
	var ands []string
	var args []any
	for _, group := range q.Groups {
		var ors []string
		for _, t := range group {
			sql, targs, err := compileTerm(t)
			if err != nil {
				return "", nil, err
			}
			if t.Not {
				sql = "NOT (" + sql + ")"
			}
			ors = append(ors, sql)
			args = append(args, targs...)
		}
		ands = append(ands, "("+strings.Join(ors, " OR ")+")")
	}
	return strings.Join(ands, " AND "), args, nil
}

func compileTerm(t query.Term) (string, []any, error) {
	switch t.Field {
	case query.Text:
		return "m.id IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)", []any{ftsPhrase(t.Value)}, nil
	case query.Tag:
		return hasTag, []any{strings.ToLower(t.Value)}, nil
	case query.Is:
		return hasTag, []any{query.IsValues[t.Value]}, nil
	case query.From:
		return "instr(lower(m.from_addr), lower(?)) > 0", []any{t.Value}, nil
	case query.To:
		return "instr(lower(m.to_addrs), lower(?)) > 0", []any{t.Value}, nil
	case query.Subject:
		return "instr(lower(m.subject), lower(?)) > 0", []any{t.Value}, nil
	case query.Account:
		return "a.name = ?", []any{t.Value}, nil
	case query.ID:
		return "m.message_id = ?", []any{t.Value}, nil
	case query.Hash:
		// Hex sorts below 'g': [prefix, prefix+"g") is the prefix range.
		return "(m.hash >= ? AND m.hash < ?)", []any{t.Value, t.Value + "g"}, nil
	case query.Date:
		var conds []string
		var args []any
		if !t.Since.IsZero() {
			conds = append(conds, "m.date >= ?")
			args = append(args, t.Since.Unix())
		}
		if !t.Before.IsZero() {
			conds = append(conds, "m.date < ?")
			args = append(args, t.Before.Unix())
		}
		// Messages with an unknown date never match a date range.
		conds = append(conds, "m.date != 0")
		return strings.Join(conds, " AND "), args, nil
	default:
		return "", nil, fmt.Errorf("unsupported field %q", t.Field)
	}
}

const hasTag = "EXISTS (SELECT 1 FROM tags t WHERE t.message_id = m.id AND t.tag = ?)"

// ftsPhrase quotes text as a single FTS5 phrase, so FTS5 operators and
// column filters in user input are never interpreted.
func ftsPhrase(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// eachPage is how many messages Each loads at a time.
const eachPage = 1000

// Each calls fn with successive pages of every message matching q, in index
// order. No database rows are held open while fn runs, so fn may use the
// index.
func (ix *Index) Each(ctx context.Context, q query.Query, fn func([]Message) error) error {
	where, args, err := compile(q)
	if err != nil {
		return err
	}
	// Keyset pagination on id is stable even if fn changes tags.
	lastID := int64(-1)
	for {
		page, err := ix.selectMessages(ctx, "("+where+") AND m.id > ?", append(slices.Clone(args), lastID), "m.id", eachPage)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		lastID = page[len(page)-1].ID
		if err := fn(page); err != nil {
			return err
		}
	}
}
