// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"cmp"
	"errors"
	"regexp"
	"strings"
)

// Folder actions.
const (
	ActionArchive = "archive"
	ActionIgnore  = "ignore"
)

// DefaultKeepDays applies when no folder rule matches or a rule omits keep_days.
const DefaultKeepDays = 7

// Policy is what sync does with one server folder.
type Policy struct {
	Action   string `json:"action"`
	KeepDays int    `json:"keep_days"` // minimum days on the server once handled
	MaxDays  int    `json:"max_days"`  // deleted after this many days even if not handled; 0: never
}

// MaxKeepDays bounds keep_days to something meaningful (100 years).
const MaxKeepDays = 36500

// Policy returns the policy of the first rule matching folder, or the default.
// Folder names use "/" as the hierarchy separator.
func (a *Account) Policy(folder string) Policy {
	for _, r := range a.Folders {
		if re, err := compileGlob(r.Match); err == nil && re.MatchString(folder) {
			return r.Policy()
		}
	}
	return FolderRule{}.Policy()
}

// Policy is the rule's policy, with defaults for what it leaves out.
func (r FolderRule) Policy() Policy {
	p := Policy{Action: cmp.Or(r.Action, ActionArchive), KeepDays: DefaultKeepDays}
	if r.KeepDays != nil {
		p.KeepDays = *r.KeepDays
	}
	if r.MaxDays != nil {
		p.MaxDays = *r.MaxDays
	}
	return p
}

func (r FolderRule) validate() error {
	if r.Match == "" {
		return errors.New("match must not be empty")
	}
	if _, err := compileGlob(r.Match); err != nil {
		return err
	}
	switch r.Action {
	case "", ActionArchive, ActionIgnore:
	default:
		return errors.New(`action must be "archive" or "ignore"`)
	}
	if r.KeepDays != nil && (*r.KeepDays < 0 || *r.KeepDays > MaxKeepDays) {
		return errors.New("keep_days must be between 0 and 36500")
	}
	if r.MaxDays != nil && (*r.MaxDays < 0 || *r.MaxDays > MaxKeepDays) {
		return errors.New("max_days must be between 0 and 36500")
	}
	if r.MaxDays != nil && *r.MaxDays > 0 && r.KeepDays != nil && *r.MaxDays < *r.KeepDays {
		return errors.New("max_days must not be less than keep_days")
	}
	return nil
}

const maxGlobLen = 256

// compileGlob turns a folder glob into an anchored, case-insensitive regexp.
// Unlike path.Match, "*" also matches "/", so "Junk*" covers "Junk/Old": a
// rule meant to ignore a folder must also cover its subfolders. Case is
// ignored because servers disagree on it ("INBOX", "Inbox", "DRAFTS"). Supported syntax: "*", "?",
// "[abc]", "[!abc]", "{a,b}" (not nested), and "\" to escape.
func compileGlob(glob string) (*regexp.Regexp, error) {
	if len(glob) > maxGlobLen {
		return nil, errors.New("match is too long")
	}
	var b strings.Builder
	b.WriteString("(?i)^")
	inBrace, inClass := false, false
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch {
		case inClass:
			if c == ']' {
				inClass = false
			}
			if c == '\\' {
				b.WriteByte('\\')
			}
			b.WriteByte(c)
		case c == '\\':
			if i+1 == len(glob) {
				return nil, errors.New("match ends with a backslash")
			}
			i++
			b.WriteString(regexp.QuoteMeta(glob[i : i+1]))
		case c == '*':
			b.WriteString(".*")
		case c == '?':
			b.WriteString(".")
		case c == '[':
			inClass = true
			b.WriteByte('[')
			if i+1 < len(glob) && glob[i+1] == '!' {
				b.WriteByte('^')
				i++
			}
		case c == '{':
			if inBrace {
				return nil, errors.New("nested braces are not supported")
			}
			inBrace = true
			b.WriteString("(?:")
		case c == '}' && inBrace:
			inBrace = false
			b.WriteByte(')')
		case c == ',' && inBrace:
			b.WriteByte('|')
		default:
			// Slicing (not string(c)) keeps multi-byte UTF-8 intact.
			b.WriteString(regexp.QuoteMeta(glob[i : i+1]))
		}
	}
	if inBrace || inClass {
		return nil, errors.New("match has an unclosed bracket or brace")
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, errors.New("match is not a valid glob")
	}
	return re, nil
}
