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
	// An OSC without its end stops at the end of the line, so it cannot
	// hide the rest of a message.
	ansiOSC = regexp.MustCompile(`\x1b\][^\x07\x1b\n]*(?:\x07|\x1b\\)?`)
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
		case unicode.IsControl(r), isBidiControl(r), isInvisible(r):
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

// isInvisible reports format characters that render as nothing: zero-width
// spaces, word joiners, BOM, soft hyphens, and Unicode tag characters (a
// way to smuggle invisible text to AI). The zero-width joiner and
// non-joiner are kept: scripts like Persian and emoji sequences need them.
func isInvisible(r rune) bool {
	switch r {
	case 0x200C, 0x200D:
		return false
	// Letters and marks that render blank: fillers for padding or spoofing.
	case 0x034F, 0x115F, 0x1160, 0x17B4, 0x17B5, 0x2800, 0x3164, 0xFFA0:
		return true
	}
	return unicode.Is(unicode.Cf, r)
}

func isBidiControl(r rune) bool {
	return r >= 0x202A && r <= 0x202E || r >= 0x2066 && r <= 0x2069 || r == 0x200E || r == 0x200F || r == 0x061C
}
