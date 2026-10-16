// SPDX-License-Identifier: AGPL-3.0-or-later

package render

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

var (
	// "label <https://...>" as plain-text mail (and many newsletters) write links.
	bracketURL = regexp.MustCompile(`[ \t]*<((?:https?|mailto):[^\s>]+)>`)
	// Bare URLs.
	bareURL = regexp.MustCompile(`\bhttps?://[^\s<>"]+`)
	// Either kind, leftmost first.
	anyURL = regexp.MustCompile(bracketURL.String() + "|" + bareURL.String())
)

// truncated ends a text that was cut at MaxTextOutput.
const truncated = "\n\n[nought shows only the first megabyte of this message]"

// longURL is the length from which a bare URL is moved to the link list.
const longURL = 40

// TextWithLinks tidies a plain-text body for reading: bracketed URLs and
// long bare URLs become numbered references (returned separately), so long
// tracking links do not bury the text. The text is sanitized; st styles
// the references (nil functions leave them plain). With a Quote style,
// "> > " quotes are shown with bars, as HTML quotes are.
func TextWithLinks(s string, st Styler) (string, []Link) {
	// The same limit as for HTML: a few megabytes of plain text (quote
	// markers, say) would take seconds to style.
	cut := len(s) > MaxTextOutput
	if cut {
		s = strings.ToValidUTF8(s[:MaxTextOutput], "")
	}
	w := plainWriter{st: st}
	lines := strings.Split(Sanitize(s), "\n")
	for i, line := range lines {
		if i > 0 {
			w.b.WriteByte('\n')
		}
		next := ""
		if i+1 < len(lines) {
			next = lines[i+1]
		}
		w.line(line, next)
	}
	text := strings.TrimRight(w.b.String(), "\n")
	if cut {
		text += truncated
	}
	return text, w.links
}

// plainWriter styles plain text line by line, numbering the links it moves
// out of the text.
type plainWriter struct {
	st    Styler
	b     strings.Builder
	links []Link
}

// line writes one line; next is the line after it (an attribution is styled
// as the deeper quote that follows it).
func (w *plainWriter) line(line, next string) {
	text := func(s string) string { return s }
	if w.st.Quote != nil {
		depth, rest := quoteDepth(line)
		if depth > 0 {
			w.b.WriteString(quoteBars(w.st, depth, rest == ""))
			line, text = rest, func(s string) string { return byWord(w.st.Dim, s) }
		}
		// An attribution ("On Fri, Bob wrote:") right above a deeper quote
		// is shown in that quote's style.
		if strings.HasSuffix(strings.TrimSpace(line), ":") && !anyURL.MatchString(line) {
			if nextDepth, _ := quoteDepth(next); next != "" && nextDepth > depth {
				w.b.WriteString(byWord(func(s string) string { return w.st.Quote(depth+1, s) }, line))
				return
			}
		}
	}
	last := 0
	for _, m := range anyURL.FindAllStringIndex(line, -1) {
		if m[0] > last {
			w.b.WriteString(text(line[last:m[0]]))
		}
		w.b.WriteString(w.link(line[m[0]:m[1]]))
		last = m[1]
	}
	if last < len(line) {
		w.b.WriteString(text(line[last:]))
	}
}

// link styles one URL: a bracketed or long one becomes a numbered reference.
func (w *plainWriter) link(m string) string {
	if sub := bracketURL.FindStringSubmatch(m); sub != nil && strings.HasSuffix(m, ">") {
		if n := w.number(sub[1], ""); n != "" {
			return apply(w.st.Dim, "["+n+"]")
		}
		return m
	}
	if len(m) < longURL {
		return apply(w.st.Link, m)
	}
	n := w.number(m, m)
	if n == "" {
		return m
	}
	host := m[strings.Index(m, "://")+3:]
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	return apply(w.st.Link, host+"/…") + apply(w.st.Dim, "["+n+"]")
}

// number is a link's reference number, reused for a repeated URL; "" when
// the list is full.
func (w *plainWriter) number(u, label string) string {
	for i, l := range w.links {
		if l.URL == u {
			return strconv.Itoa(i + 1)
		}
	}
	if len(w.links) >= maxLinks {
		return ""
	}
	w.links = append(w.links, Link{URL: u, Label: label})
	return strconv.Itoa(len(w.links))
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
// A host with non-ASCII letters is given in its ASCII (punycode) form, so
// a look-alike (a Cyrillic а in apple.com) shows as what it is.
func (l Link) Host() string {
	u, err := url.Parse(l.URL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if ascii, err := idna.ToASCII(host); err == nil {
		return ascii
	}
	return host
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
