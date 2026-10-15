// SPDX-License-Identifier: AGPL-3.0-or-later

package render

import (
	"flag"
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
