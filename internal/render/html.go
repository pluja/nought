// SPDX-License-Identifier: AGPL-3.0-or-later

package render

import (
	"net/url"
	"strconv"
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

var headings = map[atom.Atom]bool{
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
}

// Limits that keep hostile HTML cheap to convert.
const (
	MaxHTMLInput  = 4 << 20 // bytes of HTML processed
	MaxTextOutput = 1 << 20 // bytes of text produced
	maxQuoteDepth = 8       // nested quotes shown with prefixes
	maxLinks      = 500     // links listed at the end
)

// Styler decorates text for display. Every function receives text that is
// already sanitized and must return it styled (e.g. with terminal escape
// codes). A nil function leaves text plain.
type Styler struct {
	Bold, Italic, Heading, Link, Dim, Quote func(string) string
}

func apply(f func(string) string, s string) string {
	if f == nil {
		return s
	}
	return f(s)
}

// Link is a link found in a message: its target and the text it was shown
// with (empty if unknown).
type Link struct {
	URL   string `json:"url"`
	Label string `json:"label,omitempty"`
}

// HTMLToText reduces HTML to readable plain text: paragraphs, lists,
// quotes, and links as numbered references ("label[1]") with the targets
// listed at the end. Scripts, styles, forms and images (except their alt
// text) are dropped, and only http, https and mailto links are kept.
func HTMLToText(s string) string {
	text, links := HTMLToStyled(s, Styler{})
	return WithLinkList(text, links, Styler{})
}

// HTMLToStyled is HTMLToText with emphasis, headings, links and quotes
// decorated by st, returning the links separately instead of listing them.
// Nothing is ever fetched.
func HTMLToStyled(s string, st Styler) (string, []Link) {
	if len(s) > MaxHTMLInput {
		s = s[:MaxHTMLInput]
	}
	w := &textWriter{st: st}
	z := html.NewTokenizer(strings.NewReader(s))
	var skipDepth int
	var skipTag atom.Atom
	var open []int // link number of each open <a> (0: not listed)

	for {
		tt := z.Next()
		if tt == html.ErrorToken || w.b.Len() >= MaxTextOutput {
			break
		}
		tok := z.Token()
		if skipDepth > 0 {
			// HTML allows omitting </head>; the body still starts at <body>.
			if skipTag == atom.Head && tt == html.StartTagToken && tok.DataAtom == atom.Body {
				skipDepth = 0
				continue
			}
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
			w.start(a, tok, tt, &open)
		case html.EndTagToken:
			w.end(tok.DataAtom, &open)
		}
	}
	return w.result(), w.links
}

func (w *textWriter) start(a atom.Atom, tok html.Token, tt html.TokenType, open *[]int) {
	switch {
	case a == atom.Br:
		w.newline()
	case a == atom.Li:
		w.line()
		w.raw("• ")
	case a == atom.Tr || a == atom.Dt || a == atom.Dd:
		w.line()
	case a == atom.Td || a == atom.Th:
		if w.lineLen > 0 {
			w.raw("  ")
		}
	case a == atom.Img:
		if alt := strings.TrimSpace(attr(tok, "alt")); alt != "" {
			w.styled(w.st.Dim, "["+alt+"]")
		}
	case a == atom.A && tt == html.StartTagToken:
		*open = append(*open, w.addLink(safeURL(attr(tok, "href"))))
		w.link++
		w.label.Reset()
	case a == atom.B || a == atom.Strong:
		w.bold++
	case a == atom.I || a == atom.Em:
		w.italic++
	case a == atom.Pre:
		w.block()
		w.pre++
	case a == atom.Blockquote:
		w.block()
		w.quote++
	case headings[a]:
		w.block()
		w.heading++
	case blocks[a]:
		w.block()
	}
}

func (w *textWriter) end(a atom.Atom, open *[]int) {
	switch {
	case a == atom.A && len(*open) > 0:
		n := (*open)[len(*open)-1]
		*open = (*open)[:len(*open)-1]
		w.link = max(w.link-1, 0)
		if n > 0 {
			if w.links[n-1].Label == "" {
				w.links[n-1].Label = strings.TrimSpace(w.label.String())
			}
			w.space = false
			w.raw(apply(w.st.Dim, "["+strconv.Itoa(n)+"]"))
		}
	case a == atom.B || a == atom.Strong:
		w.bold = max(w.bold-1, 0)
	case a == atom.I || a == atom.Em:
		w.italic = max(w.italic-1, 0)
	case a == atom.Pre && w.pre > 0:
		w.pre--
		w.block()
	case a == atom.Blockquote && w.quote > 0:
		w.block()
		w.quote--
	case headings[a]:
		w.heading = max(w.heading-1, 0)
		w.block()
	case a == atom.Li || a == atom.Tr || a == atom.Dt || a == atom.Dd:
		w.line()
	case blocks[a]:
		w.block()
	}
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

// textWriter accumulates text, collapsing whitespace outside <pre>,
// prefixing quoted lines, and collecting links.
type textWriter struct {
	b       strings.Builder
	st      Styler
	pre     int
	quote   int
	bold    int
	italic  int
	heading int
	link    int
	lineLen int  // characters on the current line
	space   bool // a collapsed space is pending
	breaks  int  // newlines already at the end
	links   []Link
	label   strings.Builder // plain text of the link being read
}

// addLink numbers a URL (reusing the number of a repeated one); 0 means
// the link is not listed (unsafe scheme, empty, or too many).
func (w *textWriter) addLink(u string) int {
	if u == "" {
		return 0
	}
	for i, l := range w.links {
		if l.URL == u {
			return i + 1
		}
	}
	if len(w.links) >= maxLinks {
		return 0
	}
	w.links = append(w.links, Link{URL: u})
	return len(w.links)
}

// raw writes trusted text: structure produced here, or mail text that was
// already sanitized (and maybe styled).
func (w *textWriter) raw(s string) {
	for _, r := range s {
		if r == '\n' {
			w.b.WriteByte('\n')
			w.lineLen = 0
			w.breaks++
			continue
		}
		if w.lineLen == 0 && w.quote > 0 {
			w.b.WriteString(apply(w.st.Quote, strings.Repeat("│ ", min(w.quote, maxQuoteDepth))))
		}
		w.b.WriteRune(r)
		w.lineLen++
		w.breaks = 0
	}
}

// styled writes one piece of mail text as a separate word: sanitized
// first, then styled.
func (w *textWriter) styled(style func(string) string, word string) {
	if w.space && w.lineLen > 0 {
		w.raw(" ")
	}
	w.space = false
	w.raw(apply(style, SanitizeLine(word)))
}

// wordStyle sanitizes a word of mail text and applies the emphasis in effect.
func (w *textWriter) wordStyle(word string) string {
	word = SanitizeLine(word)
	switch {
	case w.link > 0:
		if w.label.Len() < 200 {
			w.label.WriteString(word + " ")
		}
		return apply(w.st.Link, word)
	case w.heading > 0:
		return apply(w.st.Heading, word)
	case w.bold > 0:
		return apply(w.st.Bold, word)
	case w.italic > 0:
		return apply(w.st.Italic, word)
	}
	return word
}

func (w *textWriter) text(s string) {
	if w.pre > 0 {
		w.raw(Sanitize(s))
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
		w.raw(w.wordStyle(f))
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

// result is the text within the size limit.
func (w *textWriter) result() string {
	out := strings.TrimSpace(w.b.String())
	if len(out) > MaxTextOutput {
		out = strings.ToValidUTF8(out[:MaxTextOutput], "")
	}
	return out
}

// WithLinkList appends a numbered list of link targets to text.
func WithLinkList(text string, links []Link, st Styler) string {
	if len(links) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString(text + "\n\n" + apply(st.Heading, "Links"))
	for i, l := range links {
		b.WriteString("\n" + apply(st.Dim, "["+strconv.Itoa(i+1)+"] "+SanitizeLine(l.URL)))
	}
	return b.String()
}
