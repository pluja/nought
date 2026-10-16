// SPDX-License-Identifier: AGPL-3.0-or-later

package render

import (
	"bytes"
	"net/url"
	"regexp"
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

// Unreadable replaces HTML that cannot be converted safely.
const Unreadable = "[nought could not convert this HTML part: it is nested too deeply]"

// Limits that keep hostile HTML cheap to convert.
const (
	MaxHTMLInput  = 4 << 20 // bytes of HTML processed
	MaxTextOutput = 1 << 20 // bytes of text produced
	maxQuoteDepth = 8       // nested quotes shown with prefixes
	maxLinks      = 500     // links listed at the end
)

// Styler decorates text for display. Every function receives text that is
// already sanitized and must return it styled (e.g. with terminal escape
// codes). A nil function leaves text plain. Quote styles one quote level
// (1 is the outermost): its bars, and the attribution line that introduces
// it ("On Fri, Bob wrote:"), so each level can have its own color; quoted
// text itself is Dim.
type Styler struct {
	Bold, Italic, Heading, Link, Dim func(string) string
	Quote                            func(level int, s string) string
}

func apply(f func(string) string, s string) string {
	if f == nil {
		return s
	}
	return f(s)
}

// quoteBars is the prefix of a line quoted depth times: a bar per level,
// each in its level's style. A blank line gets no trailing space.
func quoteBars(st Styler, depth int, blank bool) string {
	depth = min(depth, maxQuoteDepth)
	var b strings.Builder
	for level := 1; level <= depth; level++ {
		bar := "│ "
		if blank && level == depth {
			bar = "│"
		}
		if st.Quote != nil {
			bar = st.Quote(level, bar)
		}
		b.WriteString(bar)
	}
	return b.String()
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
// Nothing is ever fetched. The HTML is parsed into a tree the way a browser
// would, so hidden elements and omitted end tags are handled exactly.
func HTMLToStyled(s string, st Styler) (string, []Link) {
	if len(s) > MaxHTMLInput {
		s = s[:MaxHTMLInput]
	}
	w := &textWriter{st: st}
	// Scripting off: <noscript> content is parsed as markup (and skipped).
	doc, err := html.ParseWithOptions(strings.NewReader(s), html.ParseOptionEnableScripting(false))
	if err != nil {
		// Too deeply nested to convert. A partial conversion could show
		// text that is hidden in a mail client, so none is shown.
		return Unreadable, nil
	}
	var open []int // link number of each open <a> (0: not listed)
	w.walk(doc, &open)
	return w.result(), w.links
}

// walk converts a subtree. Its recursion is bounded: the parser rejects
// documents nested deeper than 512 elements.
func (w *textWriter) walk(n *html.Node, open *[]int) {
	if w.b.Len() >= MaxTextOutput {
		return
	}
	switch n.Type {
	case html.TextNode:
		if !w.look.hidesText() {
			w.text(n.Data)
		}
		return
	case html.ElementNode:
		d, hiddenAttr := styleOf(n)
		if skipped[n.DataAtom] || hiddenAttr || removed(d) {
			return
		}
		parent := w.look
		w.look = parent.inherit(d)
		tok := html.Token{Type: html.StartTagToken, DataAtom: n.DataAtom, Data: n.Data, Attr: n.Attr}
		w.start(n.DataAtom, tok, html.StartTagToken, open)
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			w.walk(c, open)
		}
		w.end(n.DataAtom, open)
		w.look = parent
		return
	case html.DocumentNode:
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			w.walk(c, open)
		}
	}
}

// Hidden text. Newsletters hide preheaders and duplicate mobile layouts;
// phishing and prompt injection use the same tricks to show machines text
// that people never see. Two kinds of styles hide content:
//
//   - removed: the element and everything in it is gone, whatever its
//     children say (display:none, opacity:0, a zero-size box with its
//     overflow hidden, far off-screen, the hidden attribute);
//   - inherited (font-size:0, visibility:hidden, transparent text): text is
//     hidden until a descendant sets a visible value again. Email builders
//     put font-size:0 on wrappers to remove gaps between columns.
//
// Styles in <style> blocks are not applied (there is no CSS engine), so
// class-based hiding is not detected.

// styleOf returns an element's inline style declarations, and whether it
// has the hidden attribute.
func styleOf(n *html.Node) (map[string]string, bool) {
	var d map[string]string
	hidden := false
	for _, a := range n.Attr {
		switch a.Key {
		case "hidden":
			hidden = true
		case "style":
			d = declarations(a.Val)
		}
	}
	return d, hidden
}

func removed(d map[string]string) bool {
	if d == nil {
		return false
	}
	clipped := d["overflow"] == "hidden" &&
		(isZero(d["max-height"]) || isZero(d["height"]) || isZero(d["width"]) || isZero(d["max-width"]))
	return d["display"] == "none" || isZero(d["opacity"]) || clipped ||
		offscreen(d["text-indent"]) || offscreen(d["left"]) || offscreen(d["top"]) ||
		offscreen(d["margin-left"]) || offscreen(d["margin-top"])
}

// look is the inherited state that decides whether text shows.
type look struct {
	zeroFont, invisible, transparent bool
}

func (l look) hidesText() bool { return l.zeroFont || l.invisible || l.transparent }

// inherit applies an element's declarations; values it does not set (or
// sets to "inherit") keep the parent's.
func (l look) inherit(d map[string]string) look {
	if v, ok := d["font-size"]; ok && v != "inherit" {
		l.zeroFont = isZero(v)
	}
	switch d["visibility"] {
	case "hidden", "collapse":
		l.invisible = true
	case "visible":
		l.invisible = false
	}
	if v, ok := d["color"]; ok && v != "inherit" {
		l.transparent = v == "transparent"
	}
	return l
}

var (
	cssComment   = regexp.MustCompile(`/\*.*?\*/`)
	cssImportant = regexp.MustCompile(`!\s*important\s*$`)
	cssHack      = regexp.MustCompile(`\\[0-9]+\s*$`) // e.g. "none\9", an old IE hack browsers still honour
	cssNumber    = regexp.MustCompile(`^(-?[0-9]*\.?[0-9]+)([a-z%]*)$`)
)

// declarations parses an inline style into property: value, lowercased,
// without comments, !important or hacks. Later declarations win.
func declarations(style string) map[string]string {
	out := map[string]string{}
	for _, decl := range strings.Split(cssComment.ReplaceAllString(strings.ToLower(style), ""), ";") {
		k, v, ok := strings.Cut(decl, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		v = strings.TrimSpace(cssImportant.ReplaceAllString(v, ""))
		v = strings.TrimSpace(cssHack.ReplaceAllString(v, ""))
		out[strings.TrimSpace(k)] = v
	}
	return out
}

// cssLength parses "12px", "0", "-3.5em".
func cssLength(v string) (float64, string, bool) {
	m := cssNumber.FindStringSubmatch(v)
	if m == nil {
		return 0, "", false
	}
	f, err := strconv.ParseFloat(m[1], 64)
	return f, m[2], err == nil
}

func isZero(v string) bool {
	f, _, ok := cssLength(v)
	return ok && f == 0
}

// offscreen reports a position far outside any window.
func offscreen(v string) bool {
	f, unit, ok := cssLength(v)
	switch {
	case !ok:
		return false
	case unit == "em" || unit == "rem":
		return f <= -100
	default:
		return f <= -1000
	}
}

func (w *textWriter) start(a atom.Atom, tok html.Token, tt html.TokenType, open *[]int) {
	switch {
	case a == atom.Br:
		w.newline()
	case a == atom.Li:
		w.line()
		w.raw("• ")
	case a == atom.Tr || a == atom.Dt || a == atom.Dd || a == atom.Div:
		w.line()
	case a == atom.Td || a == atom.Th:
		if w.lineLen > 0 {
			w.raw("  ")
		}
	case a == atom.Img:
		// An image shows whatever the font size; its alt text stands in.
		if alt := strings.TrimSpace(attr(tok, "alt")); alt != "" && !w.look.invisible {
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
		if w.attribution() {
			w.line() // the attribution stays right above its quote
		} else {
			w.block()
		}
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
			w.linked = true
		}
	case a == atom.B || a == atom.Strong:
		w.bold = max(w.bold-1, 0)
	case a == atom.I || a == atom.Em:
		w.italic = max(w.italic-1, 0)
	case a == atom.Pre && w.pre > 0:
		w.pre--
		w.block()
	case a == atom.Blockquote && w.quote > 0:
		w.line()
		w.quote--
		w.block()
	case headings[a]:
		w.heading = max(w.heading-1, 0)
		w.block()
	case a == atom.Li || a == atom.Tr || a == atom.Dt || a == atom.Dd || a == atom.Div:
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
	b       bytes.Buffer // a buffer: an attribution line is rewritten in its quote's style
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
	blank   int  // blank lines not written yet: their quote bars depend on the next line
	depth   int  // quote depth of the last line written
	lineAt  int  // where the text of the last line starts in b (after its bars)
	linked  bool // the last line has a link
	links   []Link
	look    look            // inherited styles of the element being converted
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
			if w.lineLen > 0 {
				w.b.WriteByte('\n')
			} else {
				w.blank++
			}
			w.lineLen = 0
			w.breaks++
			continue
		}
		if w.lineLen == 0 {
			// A blank line takes the depth both its neighbours share, so a
			// quote never ends on a bar of its own.
			for ; w.blank > 0; w.blank-- {
				w.b.WriteString(quoteBars(w.st, min(w.depth, w.quote), true) + "\n")
			}
			w.b.WriteString(quoteBars(w.st, w.quote, false))
			w.depth, w.lineAt, w.linked = w.quote, w.b.Len(), false
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
	case w.quote > 0:
		return apply(w.st.Dim, word)
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

// attribution reports whether the last line introduces the quote about to
// open ("On Fri, Bob wrote:", in any language a line ending in a colon,
// right before the quote). It is then shown in that quote's style, unless
// it has a link, whose styling would be lost.
func (w *textWriter) attribution() bool {
	if w.blank > 0 || w.breaks > 1 || w.depth != w.quote || w.b.Len() == 0 {
		return false
	}
	line := w.b.Bytes()[w.lineAt:]
	ended := len(line) > 0 && line[len(line)-1] == '\n'
	text := strings.TrimSuffix(string(line), "\n")
	plain := strings.TrimSpace(ansiCSI.ReplaceAllString(text, ""))
	if !strings.HasSuffix(plain, ":") {
		return false
	}
	if w.st.Quote != nil && !w.linked {
		w.b.Truncate(w.lineAt)
		w.b.WriteString(w.st.Quote(w.quote+1, plain))
		if ended {
			w.b.WriteByte('\n')
		}
	}
	return true
}

// newline is a <br>. Runs of them leave one blank line at most.
func (w *textWriter) newline() {
	w.space = false
	if w.breaks < 2 {
		w.raw("\n")
	}
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
