// SPDX-License-Identifier: AGPL-3.0-or-later

package compose

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Problem is something to fix before sending (Blocking) or to look at.
type Problem struct {
	Blocking bool   `json:"blocking"`
	Text     string `json:"text"`
}

// CheckInput is what Check needs; the caller supplies the I/O.
type CheckInput struct {
	Draft    Draft
	Me       Me
	ReplyAll bool
	Known    []string                         // addresses written to before, lowercased
	Attach   func(path string) (int64, error) // an attachment's size, or why it cannot be read
	// Attachments a forward carries from the original, added when the
	// message is built.
	CarriedAttachments int
	CarriedSize        int64
	SizeLimit          int64
}

// mentionsAttachment finds words that announce an attachment (English,
// Spanish, Catalan).
var mentionsAttachment = regexp.MustCompile(`(?i)\b(attach(ed|ment|ments)?|enclosed|adjunt[oa]s?|adjunts?|t'envio)\b`)

// Check lists the problems of a draft, blocking ones first.
func Check(in CheckInput) []Problem {
	d := in.Draft
	var ps []Problem
	block := func(format string, args ...any) { ps = append(ps, Problem{true, fmt.Sprintf(format, args...)}) }
	warn := func(format string, args ...any) { ps = append(ps, Problem{false, fmt.Sprintf(format, args...)}) }

	recipients := slices.Concat(d.To, d.Cc, d.Bcc)
	if len(recipients) == 0 {
		block("no recipients")
	}
	if a := address(d.From); a == "" || !slices.Contains(in.Me.Addresses, a) {
		block("From %s is not one of your addresses", d.From)
	}
	size := int64(len(d.Body)) + in.CarriedSize
	for _, p := range d.Attach {
		n, err := in.Attach(p)
		if err != nil {
			block("cannot attach %s: %v", p, err)
			continue
		}
		size += n
	}
	if in.SizeLimit > 0 && size*137/100 > in.SizeLimit { // base64 grows attachments by a third
		block("too large: %.1f MB (the limit is %.0f MB)", float64(size*137/100)/(1<<20), float64(in.SizeLimit)/(1<<20))
	}

	if strings.TrimSpace(d.Subject) == "" {
		warn("empty subject")
	}
	if len(d.Attach) == 0 && in.CarriedAttachments == 0 && mentionsAttachment.MatchString(ownText(d.Body)) {
		warn("you mention an attachment but nothing is attached")
	}
	if n := len(d.To) + len(d.Cc); in.ReplyAll && n > 10 {
		warn("reply-all to %d people", n)
	}
	for _, r := range recipients {
		a := address(r)
		if a == "" || slices.Contains(in.Known, a) || slices.Contains(in.Me.Addresses, a) {
			continue
		}
		for _, k := range in.Known {
			if within(a, k, 2) {
				warn("%s looks like %s: a typo?", a, k)
				break
			}
		}
	}
	slices.SortStableFunc(ps, func(x, y Problem) int {
		if x.Blocking == y.Blocking {
			return 0
		}
		if x.Blocking {
			return -1
		}
		return 1
	})
	return ps
}

// ownText is the body without quoted lines, the forwarded message and the
// signature: the words the user wrote.
func ownText(body string) string {
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if line == "-- " || strings.HasPrefix(line, forwardMarker) {
			break
		}
		if !strings.HasPrefix(line, ">") {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// within reports whether a and b are at most max edits apart (Levenshtein).
func within(a, b string, max int) bool {
	if a == b {
		return true
	}
	if d := len(a) - len(b); d > max || -d > max {
		return false
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		best := cur[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			best = min(best, cur[j])
		}
		if best > max {
			return false
		}
		prev, cur = cur, prev
	}
	return prev[len(b)] <= max
}
