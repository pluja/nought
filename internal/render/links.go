// SPDX-License-Identifier: AGPL-3.0-or-later

package render

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var (
	// "label <https://...>" as plain-text mail (and many newsletters) write links.
	bracketURL = regexp.MustCompile(`[ \t]*<((?:https?|mailto):[^\s>]+)>`)
	// Bare URLs.
	bareURL = regexp.MustCompile(`\bhttps?://[^\s<>"]+`)
	// Either kind, leftmost first.
	anyURL = regexp.MustCompile(bracketURL.String() + "|" + bareURL.String())
)

// longURL is the length from which a bare URL is moved to the link list.
const longURL = 40

// TextWithLinks tidies a plain-text body for reading: bracketed URLs and
// long bare URLs become numbered references (returned separately), so long
// tracking links do not bury the text. The text is sanitized; st styles
// the references (nil functions leave them plain). With a Quote style,
// "> > " quotes are shown with bars, as HTML quotes are.
func TextWithLinks(s string, st Styler) (string, []Link) {
	s = Sanitize(s)
	var links []Link
	number := func(u, label string) string {
		for i, l := range links {
			if l.URL == u {
				return strconv.Itoa(i + 1)
			}
		}
		if len(links) >= maxLinks {
			return ""
		}
		links = append(links, Link{URL: u, Label: label})
		return strconv.Itoa(len(links))
	}
	link := func(m string) string {
		if sub := bracketURL.FindStringSubmatch(m); sub != nil && strings.HasSuffix(m, ">") {
			if n := number(sub[1], ""); n != "" {
				return apply(st.Dim, "["+n+"]")
			}
			return m
		}
		if len(m) < longURL {
			return apply(st.Link, m)
		}
		n := number(m, m)
		if n == "" {
			return m
		}
		host := m[strings.Index(m, "://")+3:]
		if i := strings.IndexAny(host, "/?#"); i >= 0 {
			host = host[:i]
		}
		return apply(st.Link, host+"/…") + apply(st.Dim, "["+n+"]")
	}
	var b strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		text := func(s string) string { return s }
		if st.Quote != nil {
			depth, rest := quoteDepth(line)
			if depth > 0 {
				b.WriteString(quoteBars(st, depth, rest == ""))
				line, text = rest, func(s string) string { return apply(st.Dim, s) }
			}
		}
		last := 0
		for _, m := range anyURL.FindAllStringIndex(line, -1) {
			if m[0] > last {
				b.WriteString(text(line[last:m[0]]))
			}
			b.WriteString(link(line[m[0]:m[1]]))
			last = m[1]
		}
		if last < len(line) {
			b.WriteString(text(line[last:]))
		}
	}
	return strings.TrimRight(b.String(), "\n"), links
}

// quoteDepth reads the quote markers of a plain-text line: "> > a" and
// ">> a" are both "a" quoted twice.
func quoteDepth(line string) (depth int, rest string) {
	rest = line
	for {
		t := rest
		if depth > 0 {
			t = strings.TrimLeft(t, " ")
		}
		if !strings.HasPrefix(t, ">") {
			return depth, rest
		}
		depth++
		rest = strings.TrimPrefix(t[1:], " ")
	}
}

// Host returns the host a link leads to ("" for mailto: and junk).
func (l Link) Host() string {
	u, err := url.Parse(l.URL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// Misleading reports whether the link's text names a different site than
// the one it leads to, a common phishing trick ("paypal.com" leading to
// evil.example).
func (l Link) Misleading() bool {
	host := l.Host()
	label := strings.ToLower(strings.TrimSpace(l.Label))
	if host == "" || label == "" || strings.ContainsAny(label, " \t") || !strings.Contains(label, ".") {
		return false
	}
	shown := label
	if u, err := url.Parse(label); err == nil && u.Hostname() != "" {
		shown = strings.ToLower(u.Hostname())
	} else if i := strings.IndexAny(label, "/?#"); i >= 0 {
		shown = label[:i]
	}
	shown = strings.TrimPrefix(shown, "www.")
	h := strings.TrimPrefix(host, "www.")
	return shown != h && !strings.HasSuffix(h, "."+shown) && !strings.HasSuffix(shown, "."+h)
}
