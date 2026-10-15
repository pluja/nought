// SPDX-License-Identifier: AGPL-3.0-or-later

package render

import (
	"net/url"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// skipped elements never contribute text.
var skipped = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Head: true, atom.Title: true, atom.Noscript: true,
	atom.Template: true, atom.Svg: true, atom.Math: true, atom.Iframe: true, atom.Object: true,
	atom.Embed: true, atom.Canvas: true, atom.Select: true, atom.Button: true,
}

// blocks start on a new line.
var blocks = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Section: true, atom.Article: true, atom.Header: true,
	atom.Footer: true, atom.Table: true, atom.Ul: true, atom.Ol: true,
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
	atom.Blockquote: true, atom.Pre: true, atom.Hr: true, atom.Dl: true,
	atom.Center: true, atom.Address: true, atom.Figure: true, atom.Main: true, atom.Nav: true,
}

// MaxHTMLInput bounds the HTML that HTMLToText will process.
const MaxHTMLInput = 4 << 20

// HTMLToText reduces HTML to readable plain text. It keeps text, paragraph
// structure, list bullets, quotes and link targets, and drops everything
// else: scripts, styles, images (their alt text is kept), forms, and any
// URL scheme other than http, https and mailto.
func HTMLToText(s string) string {
	if len(s) > MaxHTMLInput {
		s = s[:MaxHTMLInput]
	}
	w := &textWriter{}
	z := html.NewTokenizer(strings.NewReader(s))
	var skipDepth int
	var skipTag atom.Atom
	var links []string // href of each open <a>
	var linkText []int // w.b.Len() when each <a> opened

	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		tok := z.Token()
		if skipDepth > 0 {
			switch {
			case tt == html.StartTagToken && tok.DataAtom == skipTag:
				skipDepth++
			case tt == html.EndTagToken && tok.DataAtom == skipTag:
				skipDepth--
			}
			continue
		}
		switch tt {
		case html.TextToken:
			w.text(tok.Data)
		case html.StartTagToken, html.SelfClosingTagToken:
			a := tok.DataAtom
			if skipped[a] {
				if tt == html.StartTagToken {
					skipDepth, skipTag = 1, a
				}
				continue
			}
			switch {
			case a == atom.Br:
				w.newline()
			case a == atom.Li:
				w.line()
				w.raw("- ")
			case a == atom.Tr || a == atom.Dt || a == atom.Dd:
				w.line()
			case a == atom.Td || a == atom.Th:
				if w.lineLen > 0 {
					w.raw(" | ")
				}
			case a == atom.Img:
				if alt := strings.TrimSpace(attr(tok, "alt")); alt != "" {
					w.text("[" + alt + "]")
				}
			case a == atom.A && tt == html.StartTagToken:
				links = append(links, safeURL(attr(tok, "href")))
				linkText = append(linkText, w.b.Len())
			case a == atom.Pre:
				w.block()
				w.pre++
			case a == atom.Blockquote:
				w.block()
				w.quote++
			case blocks[a]:
				w.block()
			}
		case html.EndTagToken:
			a := tok.DataAtom
			switch {
			case a == atom.A && len(links) > 0:
				href, start := links[len(links)-1], linkText[len(linkText)-1]
				links, linkText = links[:len(links)-1], linkText[:len(linkText)-1]
				label := strings.TrimSpace(w.b.String()[start:])
				if href != "" && href != label && "mailto:"+label != href {
					w.raw(" <" + href + ">")
				}
			case a == atom.Pre && w.pre > 0:
				w.pre--
				w.block()
			case a == atom.Blockquote && w.quote > 0:
				w.block()
				w.quote--
			case a == atom.Li || a == atom.Tr || a == atom.Dt || a == atom.Dd:
				w.line()
			case blocks[a]:
				w.block()
			}
		}
	}
	return Sanitize(w.String())
}

func attr(tok html.Token, name string) string {
	for _, a := range tok.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

// safeURL returns the URL if it uses a scheme worth showing, else "".
func safeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "mailto":
		return raw
	default:
		return ""
	}
}

// textWriter accumulates text, collapsing whitespace outside <pre> and
// prefixing quoted lines with "> ".
type textWriter struct {
	b       strings.Builder
	pre     int
	quote   int
	lineLen int  // characters on the current line
	space   bool // a collapsed space is pending
	breaks  int  // newlines already at the end
}

func (w *textWriter) raw(s string) {
	for _, r := range s {
		if r == '\n' {
			w.b.WriteByte('\n')
			w.lineLen = 0
			w.breaks++
			continue
		}
		if w.lineLen == 0 && w.quote > 0 {
			w.b.WriteString(strings.Repeat("> ", w.quote))
		}
		w.b.WriteRune(r)
		w.lineLen++
		w.breaks = 0
	}
}

func (w *textWriter) text(s string) {
	if w.pre > 0 {
		w.raw(s)
		return
	}
	first, _ := utf8.DecodeRuneInString(s)
	if s != "" && isHTMLSpace(first) {
		w.space = true
	}
	for i, f := range strings.FieldsFunc(s, isHTMLSpace) {
		if (i > 0 || w.space) && w.lineLen > 0 {
			w.raw(" ")
		}
		w.raw(f)
		w.space = false
	}
	last, _ := utf8.DecodeLastRuneInString(s)
	if s != "" && isHTMLSpace(last) {
		w.space = true
	}
}

func isHTMLSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == ' '
}

func (w *textWriter) newline() {
	w.space = false
	w.raw("\n")
}

// block ends the current line and leaves one blank line, at most.
func (w *textWriter) block() { w.ensureBreaks(2) }

// line ends the current line, if it has any text.
func (w *textWriter) line() { w.ensureBreaks(1) }

func (w *textWriter) ensureBreaks(n int) {
	w.space = false
	if w.b.Len() == 0 {
		return
	}
	for w.breaks < n {
		w.raw("\n")
	}
}

func (w *textWriter) String() string {
	return strings.TrimSpace(w.b.String())
}
