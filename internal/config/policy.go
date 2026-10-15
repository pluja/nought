// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"errors"
	"path"
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
	KeepDays int    `json:"keep_days"`
}

// Policy returns the policy of the first rule matching folder, or the default.
func (a *Account) Policy(folder string) Policy {
	for _, r := range a.Folders {
		if globMatch(r.Match, folder) {
			p := Policy{Action: ActionArchive, KeepDays: DefaultKeepDays}
			if r.Action != "" {
				p.Action = r.Action
			}
			if r.KeepDays != nil {
				p.KeepDays = *r.KeepDays
			}
			return p
		}
	}
	return Policy{Action: ActionArchive, KeepDays: DefaultKeepDays}
}

func (r FolderRule) validate() error {
	if r.Match == "" {
		return errors.New("match must not be empty")
	}
	for _, p := range expandBraces(r.Match) {
		if _, err := path.Match(p, ""); err != nil {
			return errors.New("match is not a valid glob")
		}
	}
	switch r.Action {
	case "", ActionArchive, ActionIgnore:
	default:
		return errors.New(`action must be "archive" or "ignore"`)
	}
	if r.KeepDays != nil && *r.KeepDays < 0 {
		return errors.New("keep_days must not be negative")
	}
	return nil
}

// globMatch is path.Match plus {a,b} alternatives. Invalid patterns never
// match; Validate rejects them earlier.
func globMatch(pattern, name string) bool {
	for _, p := range expandBraces(pattern) {
		if ok, err := path.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}

// expandBraces expands the first {a,b,...} group and recurses for the rest.
// Nested braces are not supported; an unclosed brace is taken literally.
func expandBraces(pattern string) []string {
	open := strings.IndexByte(pattern, '{')
	if open < 0 {
		return []string{pattern}
	}
	end := strings.IndexByte(pattern[open:], '}')
	if end < 0 {
		return []string{pattern}
	}
	end += open
	prefix, suffix := pattern[:open], pattern[end+1:]
	var out []string
	for _, alt := range strings.Split(pattern[open+1:end], ",") {
		out = append(out, expandBraces(prefix+alt+suffix)...)
	}
	return out
}
