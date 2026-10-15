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
)

// longURL is the length from which a bare URL is moved to the link list.
const longURL = 40

// TextWithLinks tidies a plain-text body for reading: bracketed URLs and
// long bare URLs become numbered references (returned separately), so long
// tracking links do not bury the text. The text is sanitized; st styles
// the references (nil functions leave them plain).
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
	var b strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		line = bracketURL.ReplaceAllStringFunc(line, func(m string) string {
			u := bracketURL.FindStringSubmatch(m)[1]
			if n := number(u, ""); n != "" {
				return apply(st.Dim, "["+n+"]")
			}
			return m
		})
		line = bareURL.ReplaceAllStringFunc(line, func(u string) string {
			if len(u) < longURL {
				return apply(st.Link, u)
			}
			n := number(u, u)
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
	return strings.TrimRight(b.String(), "\n"), links
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
