// SPDX-License-Identifier: AGPL-3.0-or-later

package render

import (
	"flag"
	"net/url"
	"os"
	"path/filepath"
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
	st := Styler{Bold: mark("b"), Italic: mark("i"), Heading: mark("h"), Link: mark("a"), Dim: mark("d"), Quote: mark("q")}
	text, links := HTMLToStyled(`<h2>News</h2><p><b>big</b> <em>soft</em> <a href="https://x.example">go there</a></p><blockquote>q</blockquote>`, st)
	got := WithLinkList(text, links, st)
	if len(links) != 1 || links[0].Label != "go there" {
		t.Errorf("links = %+v", links)
	}
	for _, want := range []string{"<h>News", "<b>big", "<i>soft", "<a>go", "<d>[1]", "<q>│ q", "<h>Links", "<d>[1] https://x.example"} {
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
