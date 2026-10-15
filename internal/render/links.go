// SPDX-License-Identifier: AGPL-3.0-or-later

package render

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	// "label <https://...>" as plain-text mail (and many newsletters) write links.
	bracketURL = regexp.MustCompile(`[ \t]*<((?:https?|mailto):[^\s>]+)>`)
	// Bare URLs.
	bareURL = regexp.MustCompile(`\bhttps?://[^\s<>"]+`)
)

// longURL is the length from which a bare URL is moved to the link list.
const longURL = 40

// TextWithLinks tidies a plain-text body for reading: bracketed URLs and
// long bare URLs become numbered references, listed at the end, so long
// tracking links do not bury the text. The text is sanitized; st styles
// the references (nil functions leave them plain).
func TextWithLinks(s string, st Styler) string {
	s = Sanitize(s)
	var links []string
	number := func(u string) string {
		for i, l := range links {
			if l == u {
				return strconv.Itoa(i + 1)
			}
		}
		if len(links) >= maxLinks {
			return ""
		}
		links = append(links, u)
		return strconv.Itoa(len(links))
	}
	var b strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		line = bracketURL.ReplaceAllStringFunc(line, func(m string) string {
			u := bracketURL.FindStringSubmatch(m)[1]
			if n := number(u); n != "" {
				return apply(st.Dim, "["+n+"]")
			}
			return m
		})
		line = bareURL.ReplaceAllStringFunc(line, func(u string) string {
			if len(u) < longURL {
				return apply(st.Link, u)
			}
			n := number(u)
			if n == "" {
				return u
			}
			host := u[strings.Index(u, "://")+3:]
			if i := strings.IndexAny(host, "/?#"); i >= 0 {
				host = host[:i]
			}
			return apply(st.Link, host+"/…") + apply(st.Dim, "["+n+"]")
		})
		b.WriteString(line)
	}
	out := strings.TrimRight(b.String(), "\n")
	if len(links) > 0 {
		out += "\n\n" + linkList(links, st)
	}
	return out
}
