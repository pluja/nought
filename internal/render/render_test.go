// SPDX-License-Identifier: AGPL-3.0-or-later

package render

import (
	"flag"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "update golden files")

func TestHTMLToTextGolden(t *testing.T) {
	inputs, err := filepath.Glob(filepath.Join("testdata", "*.html"))
	if err != nil || len(inputs) == 0 {
		t.Fatalf("no inputs: %v", err)
	}
	for _, in := range inputs {
		t.Run(filepath.Base(in), func(t *testing.T) {
			src, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			got := HTMLToText(string(src)) + "\n"
			golden := strings.TrimSuffix(in, ".html") + ".txt"
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("HTMLToText(%s) =\n%s\nwant\n%s", in, got, want)
			}
		})
	}
}

func TestHTMLToTextDropsUnsafe(t *testing.T) {
	got := HTMLToText(`<p>a<script>evil()</script><style>x</style>b</p><a href="data:text/html,x">link</a>` +
		`<div>&#x1b;[31mred&#x1b;[0m</div><iframe><p>inside</p></iframe>`)
	for _, bad := range []string{"evil", "data:", "\x1b", "inside", "x</style>"} {
		if strings.Contains(got, bad) {
			t.Errorf("output contains %q: %q", bad, got)
		}
	}
	if !strings.Contains(got, "link") || !strings.Contains(got, "red") {
		t.Errorf("lost text: %q", got)
	}
}

func TestHTMLToTextNestedSkip(t *testing.T) {
	got := HTMLToText(`<svg><svg><text>a</text></svg><text>b</text></svg>after`)
	if got != "after" {
		t.Errorf("got %q", got)
	}
}

func TestHTMLToTextUnclosedHead(t *testing.T) {
	got := HTMLToText(`<html><head><meta charset="utf-8"><title>t</title><body><p>hello body</p>`)
	if got != "hello body" {
		t.Errorf("got %q", got)
	}
}

func TestHTMLToTextBounded(t *testing.T) {
	nested := strings.Repeat("<blockquote>", 100000) + strings.Repeat("a<br>", 20000)
	got := HTMLToText(nested)
	if len(got) > MaxTextOutput {
		t.Fatalf("output %d bytes", len(got))
	}
	if strings.Contains(got, strings.Repeat("> ", maxQuoteDepth+1)) {
		t.Error("quote depth not capped")
	}
	big := HTMLToText(strings.Repeat("<p>word</p>", 1<<20))
	if len(big) > MaxTextOutput {
		t.Errorf("output %d bytes", len(big))
	}
}

func TestSanitize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain", "plain"},
		{"red \x1b[31mtext\x1b[0m", "red text"},
		{"title \x1b]0;pwned\x07done", "title done"},
		{"title \x1b]8;;https://evil\x1b\\link", "title link"},
		{"bell\x07 and nul\x00", "bell and nul"},
		{"cr\roverwrite", "croverwrite"},
		{"bidi \u202eevil\u202c", "bidi evil"},
		{"keep\nnew\tlines", "keep\nnew\tlines"},
		{"c1 \u009b31m", "c1 31m"},
		{"bad \xff utf8", "bad � utf8"},
	}
	for _, tt := range tests {
		if got := Sanitize(tt.in); got != tt.want {
			t.Errorf("Sanitize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if got := SanitizeLine("a\n b\tc  "); got != "a b c" {
		t.Errorf("SanitizeLine = %q", got)
	}
}

func FuzzHTMLToText(f *testing.F) {
	f.Add("<p>hi <a href='https://x'>x</a></p>")
	f.Add("<blockquote><pre>a\n b</pre></blockquote><li>x")
	f.Fuzz(func(t *testing.T, s string) {
		out := HTMLToText(s)
		if strings.ContainsAny(out, "\x1b\r\x00") {
			t.Fatalf("unsafe output %q", out)
		}
	})
}

func TestTextWithLinks(t *testing.T) {
	in := "Re-subscribe here <https://email.mg-d1.substack.com/c/very-long-tracking-token-abcdef1234567890>.\n" +
		"See https://example.org/a or the long https://news.example.com/track?u=1234567890abcdefghijklmnop\n" +
		"Again <https://email.mg-d1.substack.com/c/very-long-tracking-token-abcdef1234567890>"
	text, links := TextWithLinks(in, Styler{})
	got := WithLinkList(text, links, Styler{})
	want := "Re-subscribe here[1].\n" +
		"See https://example.org/a or the long news.example.com/…[2]\n" +
		"Again[1]\n\n" +
		"Links\n[1] https://email.mg-d1.substack.com/c/very-long-tracking-token-abcdef1234567890\n" +
		"[2] https://news.example.com/track?u=1234567890abcdefghijklmnop"
	if got != want {
		t.Errorf("TextWithLinks =\n%s\nwant\n%s", got, want)
	}
	if got, _ := TextWithLinks("no links\x1b[31m here", Styler{}); got != "no links here" {
		t.Errorf("sanitize: %q", got)
	}
}

func TestHTMLToStyled(t *testing.T) {
	mark := func(tag string) func(string) string { return func(s string) string { return "<" + tag + ">" + s } }
	st := Styler{Bold: mark("b"), Italic: mark("i"), Heading: mark("h"), Link: mark("a"), Dim: mark("d"), Quote: quoteMark}
	text, links := HTMLToStyled(`<h2>News</h2><p><b>big</b> <em>soft</em> <a href="https://x.example">go there</a></p><blockquote>q</blockquote>`, st)
	got := WithLinkList(text, links, st)
	if len(links) != 1 || links[0].Label != "go there" {
		t.Errorf("links = %+v", links)
	}
	for _, want := range []string{"<h>News", "<b>big", "<i>soft", "<a>go", "<d>[1]", "<q1>│ <d>q", "<h>Links", "<d>[1] https://x.example"} {
		if !strings.Contains(got, want) {
			t.Errorf("styled output lacks %q:\n%s", want, got)
		}
	}
	// Styling never sees raw control characters.
	got, _ = HTMLToStyled("<b>a\x1b]0;x\x07b</b>", Styler{Bold: func(s string) string {
		if strings.ContainsAny(s, "\x1b\x07") {
			t.Errorf("unsanitized text reached the styler: %q", s)
		}
		return s
	}})
	if strings.Contains(got, "\x1b") {
		t.Errorf("escape in output: %q", got)
	}
}

func TestRepeatedLinksShareANumber(t *testing.T) {
	got := HTMLToText(`<a href="https://x.example">a</a> <a href="https://x.example">b</a> <a href="javascript:x()">c</a>`)
	if !strings.Contains(got, "a[1] b[1] c") || strings.Contains(got, "[2]") || strings.Contains(got, "javascript") {
		t.Errorf("got %q", got)
	}
}

func TestMisleadingLinks(t *testing.T) {
	tests := []struct {
		link Link
		want bool
	}{
		{Link{URL: "https://evil.example/login", Label: "paypal.com"}, true},
		{Link{URL: "https://evil.example/login", Label: "https://www.paypal.com/signin"}, true},
		{Link{URL: "https://www.paypal.com/signin", Label: "paypal.com"}, false},
		{Link{URL: "https://email.substack.com/c/x", Label: "substack.com"}, false},
		{Link{URL: "https://evil.example", Label: "click here"}, false},
		{Link{URL: "mailto:a@b.example", Label: "write to us"}, false},
	}
	for _, tt := range tests {
		if got := tt.link.Misleading(); got != tt.want {
			t.Errorf("Misleading(%+v) = %v, want %v", tt.link, got, tt.want)
		}
	}
}

func TestCleanURL(t *testing.T) {
	tests := []struct {
		in        string
		want      string
		removed   string
		unwrapped string
		opaque    bool
	}{
		{"https://shop.example/item?id=7&utm_source=news&utm_medium=email&fbclid=x", "https://shop.example/item?id=7", "fbclid utm_medium utm_source", "", false},
		{"https://example.org/post", "https://example.org/post", "", "", false},
		{"https://example.org/s?b=2&a=1", "https://example.org/s?b=2&a=1", "", "", false},
		{"https://click.track.example/r?url=https%3A%2F%2Freal.example%2Fa%3Fid%3D1%26utm_campaign%3Dx", "https://real.example/a?id=1", "utm_campaign", "click.track.example", false},
		{"https://email.mg-d1.substack.com/c/eJxMkk2O2zAMhU9DeVfLtFXMXJkjnPXGJjJ7O1Fz", "https://email.mg-d1.substack.com/c/eJxMkk2O2zAMhU9DeVfLtFXMXJkjnPXGJjJ7O1Fz", "", "", true},
		{"https://example.org/#utm_source=x", "https://example.org/", "", "", false},
		{"mailto:a@example.org?subject=hi", "mailto:a@example.org?subject=hi", "", "", false},
		{"javascript:alert(1)", "javascript:alert(1)", "", "", false},
	}
	for _, tt := range tests {
		got := CleanURL(tt.in)
		if got.URL != tt.want || strings.Join(got.Removed, " ") != tt.removed || got.Unwrapped != tt.unwrapped || got.Opaque != tt.opaque {
			t.Errorf("CleanURL(%q) = %+v", tt.in, got)
		}
	}
	// A redirect pointing to a non-web scheme is not followed.
	if got := CleanURL("https://r.example/?url=javascript%3Aalert(1)"); got.Unwrapped != "" || strings.Contains(got.URL, "javascript") && !strings.HasPrefix(got.URL, "https://r.example") {
		t.Errorf("followed a javascript redirect: %+v", got)
	}
}

func FuzzCleanURL(f *testing.F) {
	f.Add("https://a.example/?url=https%3A%2F%2Fb.example%2F%3Futm_x%3D1")
	f.Fuzz(func(t *testing.T, s string) {
		got := CleanURL(s)
		if got.Changed(s) {
			u, err := url.Parse(got.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				t.Fatalf("cleaned %q into a non-web URL %q", s, got.URL)
			}
		}
	})
}

func TestHTMLToTextDropsHiddenText(t *testing.T) {
	got := HTMLToText(`<h1>Big news</h1><p style="color:#fff;display:none;text-align:left">&nbsp;<strong>preheader junk</strong></p>` +
		`<p style="DISPLAY: NONE">caps</p><div hidden>attr</div><span style="visibility:hidden">invisible</span>` +
		`<table><tr class="bu" style="display:none;mso-hide:all"><td><p>mobile copy</p></td></tr></table>` +
		`<div style="display:none"><div>nested</div><img alt="pic"><br>still hidden</div>` +
		`<p style="max-height:0;overflow:hidden">squeezed</p>` +
		`<div style="mso-hide:all">outlook only</div>` +
		`<p style="display:block">shown</p><img style="display:none" alt="hidden pic">after`)
	for _, bad := range []string{"junk", "caps", "attr", "invisible", "mobile", "nested", "pic", "still hidden", "squeezed"} {
		if strings.Contains(got, bad) {
			t.Errorf("hidden text %q shown: %q", bad, got)
		}
	}
	for _, want := range []string{"Big news", "shown", "after", "outlook only"} { // mso-hide hides in Outlook only
		if !strings.Contains(got, want) {
			t.Errorf("lost %q: %q", want, got)
		}
	}
}

// An unclosed hidden <p> ends where a browser ends it, not at the end of
// the message.
func TestHiddenParagraphWithoutEndTag(t *testing.T) {
	got := HTMLToText(`<div><p style="display:none">secret<p>visible one</div><p>visible two`)
	if strings.Contains(got, "secret") || !strings.Contains(got, "visible one") || !strings.Contains(got, "visible two") {
		t.Errorf("got %q", got)
	}
}

func TestDeepNestingIsBounded(t *testing.T) {
	if got := HTMLToText(strings.Repeat("<div>", 5000) + "deep" + "<p>x"); got != Unreadable {
		t.Errorf("got %q", got)
	}
}

func TestHiddenStyleVariants(t *testing.T) {
	for _, style := range []string{
		"font-size:0", "font-size: 0px", "opacity:0", "opacity: 0.0",
		"height:0;overflow:hidden", "width:0px; overflow:hidden", "max-width:0;overflow:hidden",
		"text-indent:-9999px", "position:absolute;left:-5000px", "margin-left:-3000px",
		"color:transparent", "display:/**/none", "display:none ! important", `display:none\9`,
		"DISPLAY : NONE", "visibility:collapse",
	} {
		got := HTMLToText(`<p>shown</p><div style="` + style + `">secret</div>`)
		if strings.Contains(got, "secret") || !strings.Contains(got, "shown") {
			t.Errorf("style %q: %q", style, got)
		}
	}
	// Ordinary styles keep their text: white text is normal on dark
	// backgrounds, and Outlook-only hiding hides nothing elsewhere.
	for _, style := range []string{"color:#fff", "mso-hide:all", "height:0", "font-size:10px",
		"opacity:0.5", "left:-10px", "text-indent:-1em", "max-height:0px"} {
		if got := HTMLToText(`<div style="` + style + `">kept</div>`); got != "kept" {
			t.Errorf("style %q hid text: %q", style, got)
		}
	}
}

// Nesting deeper than the parser accepts (512) is refused as a whole: a
// partial conversion might show hidden text.
func TestTooDeepHTMLIsRefused(t *testing.T) {
	got := HTMLToText(strings.Repeat("<div>", 600) + "DEEP<p style=\"display:none\">secret</p>")
	if got != Unreadable {
		t.Errorf("got %q", got)
	}
}

func TestSanitizeDropsInvisibleCharacters(t *testing.T) {
	// Zero-width spaces, word joiner, BOM, soft hyphen and Unicode tag
	// characters (used to smuggle invisible instructions to AI).
	in := "bunny.\ufeffnet a\u200bb\u2060c\u00add" + string(rune(0xE0041)) + string(rune(0xE0042)) + "e"
	if got := Sanitize(in); got != "bunny.net abcde" {
		t.Errorf("Sanitize = %q", got)
	}
	// Joiners shape scripts and emoji: kept.
	if got := Sanitize("می\u200cخواهم 👩\u200d💻"); got != "می\u200cخواهم 👩\u200d💻" {
		t.Errorf("joiners dropped: %q", got)
	}
}

// Email builders (MJML, Klaviyo) set font-size:0 on wrappers to remove gaps
// between columns, and a real size on the text inside: inherited styles
// hide only text that does not override them.
func TestInheritedHidingCanBeOverridden(t *testing.T) {
	got := HTMLToText(`<div style="font-size:0px"><table><tr><td style="font-size:0px">` +
		`<img alt="Logo"><div style="font-size:18px;color:#FFFFFF"><span style="color: rgb(0, 0, 0)">Headline</span></div>` +
		`gap</td></tr></table></div>` +
		`<div style="visibility:hidden">no<p style="visibility:visible">yes</p></div>` +
		`<div style="color:transparent">clear<span style="color:#000">dark</span></div>` +
		`<div style="opacity:0">gone<p style="opacity:1">still gone</p></div>` +
		`<div style="display:none">none<p style="display:block">still none</p></div>`)
	for _, want := range []string{"[Logo]", "Headline", "yes", "dark"} {
		if !strings.Contains(got, want) {
			t.Errorf("lost %q: %q", want, got)
		}
	}
	for _, bad := range []string{"gap", "no", "clear", "gone", "none"} {
		if strings.Contains(got, bad) {
			t.Errorf("hidden %q shown: %q", bad, got)
		}
	}
}

func TestEmailBuilderLayout(t *testing.T) {
	cell := func(class, style, inner string) string {
		return `<div class="mj-column-per-100 ` + class + `" style="` + style + `"><table><tbody><tr>` +
			`<td style="font-size:0px;word-break:break-word;">` + inner + `</td></tr></tbody></table></div>`
	}
	text := func(s string) string {
		return `<div style="font-family:Lato;font-size:18px;color:#FFFFFF;"><p><span style="color: rgb(0, 0, 0); font-size: 16px;">` +
			s + `</span></p></div>`
	}
	html := `<div style="display:none;font-size:1px;max-height:0px;opacity:0;overflow:hidden;">Preheader&#x2007;&#x34F; &#xAD;</div>` +
		`<div class="root-container"><div class="root-container-spacing"><table><tbody><tr><td>` +
		`<div style="margin:0px auto;max-width:640px;"><table><tbody><tr><td style="direction:ltr;font-size:0px;padding:0px;">` +
		cell("desktop-only", "font-size:0px;text-align:left;", `<a href="https://example.org/a"><img alt="Big banner" src="https://cdn.example/b.png"></a>`) +
		cell("mobile-only", "display:none; font-size:0;", text("Mobile headline")) +
		cell("desktop-only", "font-size:0px;", text("Desktop headline")) +
		cell("", "font-size:0px;", text("The body text of the offer.")) +
		cell("", "font-size:0px;", `<table><tbody><tr><td style="color:#FFFFFF;font-size:16px;">`+
			`<a style="color:#FFF" href="https://example.org/buy">Buy now</a></td></tr></tbody></table>`) +
		`</td></tr></tbody></table></div></td></tr></tbody></table></div></div>`
	got := HTMLToText(html)
	for _, want := range []string{"[Big banner][1]", "Desktop headline", "The body text of the offer.", "Buy now[2]",
		"[1] https://example.org/a", "[2] https://example.org/buy"} {
		if !strings.Contains(got, want) {
			t.Errorf("lost %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"Preheader", "Mobile"} {
		if strings.Contains(got, bad) {
			t.Errorf("hidden %q shown:\n%s", bad, got)
		}
	}
}

// quoteMark shows each quote level's style as <qN>.
func quoteMark(level int, s string) string { return "<q" + strconv.Itoa(level) + ">" + s }

// Webmail (Tuta, Gmail, Outlook) puts each line in a <div>: lines, not
// paragraphs. Quote bars run through blank lines inside a quote, at the
// depth the lines around them share, and never trail a quote.
func TestQuotedRepliesReadEasily(t *testing.T) {
	in := `<div>I always like to test.</div><div><br></div><div><br></div><div><br></div><div>Oct 16 by a:</div>` +
		`<blockquote><div>nice nice</div><div><br></div><div>On Fri, me wrote:</div>` +
		`<blockquote><div>Good to know.</div><div>--</div></blockquote></blockquote>`
	want := "I always like to test.\n\nOct 16 by a:\n│ nice nice\n│\n│ On Fri, me wrote:\n│ │ Good to know.\n│ │ --"
	if got := HTMLToText(in); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Plain-text quotes ("> > ") look the same as HTML ones once styled; with
// no quote style (the CLI), they are left as written.
func TestPlainQuotes(t *testing.T) {
	in := "Thanks!\n\nOn Fri, Bob wrote:\n> Sure.\n>\n> > Lunch?\n>> Pizza http://x.example/a\n"
	mark := func(tag string) func(string) string { return func(s string) string { return "<" + tag + ">" + s } }
	got, _ := TextWithLinks(in, Styler{Link: mark("a"), Dim: mark("d"), Quote: quoteMark})
	want := "Thanks!\n\n<q1>On Fri, Bob wrote:\n<q1>│ <d>Sure.\n<q1>│\n<q1>│ <q2>│ <d>Lunch?\n<q1>│ <q2>│ <d>Pizza <a>http://x.example/a"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if plain, _ := TextWithLinks(in, Styler{}); !strings.Contains(plain, "> > Lunch?") {
		t.Errorf("unstyled quotes changed:\n%s", plain)
	}
}

// An attribution ("On Fri, Bob wrote:", any language: a line ending in a
// colon) sits right above the quote it introduces, in that quote's color.
func TestAttributionIntroducesItsQuote(t *testing.T) {
	st := Styler{Quote: quoteMark}
	tests := []struct{ in, want string }{
		{`<div>Oct 16 by a:<br></div><blockquote><div>nice</div><div>On Fri, me wrote:</div><blockquote><div>Good</div></blockquote></blockquote>`,
			"<q1>Oct 16 by a:\n<q1>│ nice\n<q1>│ <q2>On Fri, me wrote:\n<q1>│ <q2>│ Good"},
		// A linked attribution keeps its own styling, but still attaches.
		{`<div><a href="mailto:a@x.org">Ann</a> a écrit :</div><blockquote>salut</blockquote>`,
			"Ann[1] a écrit :\n<q1>│ salut"},
		// Any other paragraph keeps its distance.
		{`<p>Hello</p><blockquote>q</blockquote>`, "Hello\n\n<q1>│ q"},
	}
	for _, tt := range tests {
		if got, _ := HTMLToStyled(tt.in, st); got != tt.want {
			t.Errorf("%s:\ngot:\n%s\nwant:\n%s", tt.in, got, tt.want)
		}
	}
	if got := HTMLToText(tests[0].in); !strings.HasPrefix(got, "Oct 16 by a:\n│ nice\n│ On Fri, me wrote:\n│ │ Good") {
		t.Errorf("unstyled:\n%s", got)
	}
	plain, _ := TextWithLinks("> Bob wrote:\n> > hi\n", st)
	if plain != "<q1>│ <q2>Bob wrote:\n<q1>│ <q2>│ hi" {
		t.Errorf("plain:\n%s", plain)
	}
}
