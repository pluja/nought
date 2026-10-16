// SPDX-License-Identifier: AGPL-3.0-or-later

package compose

import (
	"reflect"
	"slices"
	"testing"
)

func TestRenderParseRoundTrip(t *testing.T) {
	d := Draft{From: "me@example.org", To: []string{"Alice <alice@example.org>", "bob@example.org"}, Subject: "Hi",
		Attach: []string{"~/a b.pdf"}, Body: "Hello\n\n-- \nme\n", InReplyTo: "m@x", References: []string{"m@x"}}
	got, err := Parse(Render(d), d)
	if err != nil || !reflect.DeepEqual(got, d) {
		t.Errorf("round trip = %+v, %v\nwant %+v", got, err, d)
	}
}

func TestParseEditedFile(t *testing.T) {
	text := "From: me@example.org\nTo: a@example.org, \"B, the second\" <b@example.org>\nCc:\nX-Evil: 1\nSubject: Hi\n" +
		"Attach: /tmp/x.pdf\nAttach: ~/y z.txt, /tmp/w\n# a comment\n\nBody line\n# not a comment in the body\n"
	d, err := Parse(text, Draft{InReplyTo: "keep@x"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.To, []string{"a@example.org", "\"B, the second\" <b@example.org>"}) || len(d.Cc) != 0 || d.Subject != "Hi" ||
		!slices.Equal(d.Attach, []string{"/tmp/x.pdf", "~/y z.txt", "/tmp/w"}) || d.InReplyTo != "keep@x" ||
		d.Body != "Body line\n# not a comment in the body\n" {
		t.Errorf("parsed = %+v", d)
	}
	if _, err := Parse("To: a@b.c\nSubject: x\r\nBcc: e@x\n\nbody", Draft{}); err == nil {
		t.Error("a CR in the header block was accepted")
	}
}

func FuzzParse(f *testing.F) {
	f.Add(Render(Draft{From: "me@example.org", To: []string{"a@b.c"}, Subject: "s", Body: "b\n"}))
	f.Add("To: x\n\nbody")
	f.Fuzz(func(t *testing.T, text string) {
		d, err := Parse(text, Draft{})
		if err != nil {
			return
		}
		again, err := Parse(Render(d), Draft{})
		if err != nil {
			t.Fatalf("a parsed draft does not parse after rendering: %v", err)
		}
		if again.Subject != d.Subject || !slices.Equal(again.To, d.To) || again.Body != d.Body {
			t.Fatalf("round trip changed it:\n%+v\n%+v", d, again)
		}
	})
}
