// SPDX-License-Identifier: AGPL-3.0-or-later

// Package render turns hostile mail content into safe plain text. HTML is
// never rendered: it is tokenized and reduced to text, links are shown as
// URLs, and nothing is ever fetched. Every string shown in a terminal passes
// through Sanitize first.
package render

import (
	"regexp"
	"strings"
	"unicode"
)

var (
	// CSI (ESC [ ... final) and OSC (ESC ] ... BEL or ESC \) sequences.
	ansiCSI = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	ansiOSC = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)?`)
)

// Sanitize makes text safe to print to a terminal: it removes terminal
// escape sequences, control characters (except newline and tab), carriage
// returns, and bidirectional overrides that can disguise text.
func Sanitize(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = ansiOSC.ReplaceAllString(s, "")
	s = ansiCSI.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r), isBidiControl(r):
			return -1
		default:
			return r
		}
	}, s)
}

// SanitizeLine is Sanitize for single-line fields (subjects, addresses):
// newlines and tabs become spaces.
func SanitizeLine(s string) string {
	return strings.Join(strings.Fields(Sanitize(s)), " ")
}

func isBidiControl(r rune) bool {
	return r >= 0x202A && r <= 0x202E || r >= 0x2066 && r <= 0x2069 || r == 0x200E || r == 0x200F || r == 0x061C
}
