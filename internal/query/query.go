// SPDX-License-Identifier: AGPL-3.0-or-later

// Package query parses nought's search language into an AST. It knows
// nothing about SQL; the index compiles the AST into parameterized queries.
//
// Syntax: terms separated by spaces are ANDed; "a OR b" ORs two terms (OR
// binds tighter than the implicit AND); a leading "-" negates a term.
// Terms are field:value pairs or bare words (full-text search), and values
// may be "double quoted" (with \" and \\ escapes).
//
//	tag:todo from:bob -is:unread "quarterly report" date:2026-01-01..2026-03-31
//
// A bare * matches every message ("*" in quotes is the text). Actions
// refuse an empty query, so * is how a rule or a command says "everything".
//
// Fields: tag, from, to (To and Cc), subject, account, id (Message-ID),
// is (unread, flagged, inbox, replied), date (YYYY-MM-DD, A..B, A.., ..B),
// and hash (a message ID prefix as shown by nou).
package query

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Fields.
const (
	Text    = "text" // bare words: full-text search
	Tag     = "tag"
	From    = "from"
	To      = "to"
	Subject = "subject"
	Account = "account"
	ID      = "id" // Message-ID
	Is      = "is"
	Date    = "date"
	Hash    = "hash" // prefix of the content hash, i.e. a message ID shown by nou
	All     = "*"    // every message
)

var fields = map[string]bool{Tag: true, From: true, To: true, Subject: true, Account: true, ID: true, Is: true, Date: true, Hash: true}

// MinHashPrefix is the shortest accepted hash: prefix.
const MinHashPrefix = 6

// IsValues maps is:<value> to the tag it stands for.
var IsValues = map[string]string{"unread": "unread", "flagged": "flagged", "inbox": "inbox", "replied": "replied"}

// Term is one condition.
type Term struct {
	Field string
	Value string
	Not   bool
	// Since and Before bound date terms: Since <= date < Before. Zero
	// values mean unbounded.
	Since, Before time.Time
}

// Query is an AND of groups; each group is an OR of terms. The empty query
// matches everything.
type Query struct {
	Groups [][]Term
}

// MaxLen and MaxTerms bound query size so hostile input cannot cost much
// (and stays far below SQLite's expression depth limit).
const (
	MaxLen   = 4096
	MaxTerms = 64
)

// Parse parses s.
func Parse(s string) (Query, error) {
	if len(s) > MaxLen {
		return Query{}, errors.New("query is too long")
	}
	if !utf8.ValidString(s) {
		return Query{}, errors.New("query is not valid UTF-8")
	}
	if strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) && !unicode.IsSpace(r) }) >= 0 {
		return Query{}, errors.New("query contains control characters")
	}
	toks, err := tokenize(s)
	if err != nil {
		return Query{}, err
	}
	var q Query
	var group []Term
	expectTerm, terms := true, 0
	for _, tok := range toks {
		if tok.or {
			if expectTerm {
				return Query{}, errors.New("OR must be between two terms")
			}
			expectTerm = true
			continue
		}
		t, err := parseTerm(tok)
		if err != nil {
			return Query{}, err
		}
		if terms++; terms > MaxTerms {
			return Query{}, fmt.Errorf("query has more than %d terms", MaxTerms)
		}
		if !expectTerm { // implicit AND: close the current group
			q.Groups = append(q.Groups, group)
			group = nil
		}
		group = append(group, t)
		expectTerm = false
	}
	if expectTerm && len(toks) > 0 {
		return Query{}, errors.New("OR must be between two terms")
	}
	if len(group) > 0 {
		q.Groups = append(q.Groups, group)
	}
	return q, nil
}

type token struct {
	text    string
	quoted  bool // some part was quoted, so the token is not the OR operator
	literal bool // the term itself was quoted ("tag:x" or -"x"): plain text
	negated bool // a literal term preceded by "-"
	or      bool
}

func tokenize(s string) ([]token, error) {
	var toks []token
	var b strings.Builder
	inQuote, quoted, literal, negated, started := false, false, false, false, false
	flush := func() {
		if !started {
			return
		}
		text := b.String()
		toks = append(toks, token{text: text, quoted: quoted, literal: literal, negated: negated, or: !quoted && text == "OR"})
		b.Reset()
		quoted, literal, negated, started = false, false, false, false
	}
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case inQuote && r == '\\' && i+1 < len(runes):
			i++
			b.WriteRune(runes[i])
		case r == '"':
			if !inQuote && !literal && (b.Len() == 0 || b.String() == "-") {
				literal, negated = true, b.Len() == 1
				b.Reset()
			}
			inQuote = !inQuote
			quoted, started = true, true
		case !inQuote && unicode.IsSpace(r):
			flush()
		default:
			b.WriteRune(r)
			started = true
		}
	}
	if inQuote {
		return nil, errors.New("unterminated quote")
	}
	flush()
	return toks, nil
}

func parseTerm(tok token) (Term, error) {
	text := tok.text
	if tok.literal {
		if text == "" {
			return Term{}, errors.New("empty term")
		}
		return Term{Field: Text, Value: text, Not: tok.negated}, nil
	}
	var t Term
	if strings.HasPrefix(text, "-") && len(text) > 1 {
		t.Not = true
		text = text[1:]
	}
	if text == All {
		if t.Not {
			return Term{}, errors.New("-* matches nothing")
		}
		return Term{Field: All}, nil
	}
	field, value, hasField := strings.Cut(text, ":")
	if !hasField || field == "" {
		t.Field, t.Value = Text, text
		return t, nil
	}
	field = strings.ToLower(field)
	if !fields[field] {
		return Term{}, fmt.Errorf("unknown field %q (quote the term to search for it as text)", field)
	}
	if value == "" {
		return Term{}, fmt.Errorf("%s: needs a value", field)
	}
	t.Field, t.Value = field, value
	switch field {
	case Is:
		if _, ok := IsValues[strings.ToLower(value)]; !ok {
			return Term{}, fmt.Errorf("is:%s: expected one of unread, flagged, inbox, replied", value)
		}
		t.Value = strings.ToLower(value)
	case ID:
		t.Value = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(value), "<"), ">")
		if t.Value == "" {
			return Term{}, errors.New("id: needs a value")
		}
	case Hash:
		t.Value = strings.ToLower(value)
		if len(t.Value) < MinHashPrefix || len(t.Value) > 64 || strings.Trim(t.Value, "0123456789abcdef") != "" {
			return Term{}, fmt.Errorf("hash:%s: expected %d to 64 hex characters", value, MinHashPrefix)
		}
	case Date:
		since, before, err := parseDateRange(value)
		if err != nil {
			return Term{}, err
		}
		t.Since, t.Before = since, before
	}
	return t, nil
}

const dateLayout = "2006-01-02"

// parseDateRange accepts "A", "A..B", "A.." and "..B" (dates in UTC, both
// ends inclusive).
func parseDateRange(v string) (since, before time.Time, err error) {
	from, to, isRange := strings.Cut(v, "..")
	if !isRange {
		to = from
	}
	if from == "" && to == "" {
		return since, before, errors.New("date: needs at least one end")
	}
	if from != "" {
		if since, err = time.Parse(dateLayout, from); err != nil {
			return since, before, fmt.Errorf("date: %q is not YYYY-MM-DD", from)
		}
	}
	if to != "" {
		end, err := time.Parse(dateLayout, to)
		if err != nil {
			return since, before, fmt.Errorf("date: %q is not YYYY-MM-DD", to)
		}
		before = end.AddDate(0, 0, 1)
	}
	if !since.IsZero() && !before.IsZero() && !since.Before(before) {
		return since, before, errors.New("date: range ends before it starts")
	}
	return since, before, nil
}

// String formats q so that Parse(q.String()) is equivalent to q.
func (q Query) String() string {
	groups := make([]string, 0, len(q.Groups))
	for _, g := range q.Groups {
		terms := make([]string, 0, len(g))
		for _, t := range g {
			terms = append(terms, t.String())
		}
		groups = append(groups, strings.Join(terms, " OR "))
	}
	return strings.Join(groups, " ")
}

// String formats one term, quoting the value when needed.
func (t Term) String() string {
	var b strings.Builder
	if t.Field == All {
		return All
	}
	if t.Not {
		b.WriteByte('-')
	}
	if t.Field != Text {
		b.WriteString(t.Field + ":")
	}
	b.WriteString(quote(t.Value))
	return b.String()
}

func quote(v string) string {
	if v != "" && v != "OR" && v != All && !strings.ContainsAny(v, " \t\r\n\"\\:") && !strings.HasPrefix(v, "-") &&
		strings.IndexFunc(v, unicode.IsSpace) < 0 {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(v) + `"`
}
