// SPDX-License-Identifier: AGPL-3.0-or-later

package mailmsg

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func crlf(s string) []byte { return []byte(strings.ReplaceAll(s, "\n", "\r\n")) }

func TestParse(t *testing.T) {
	raw := crlf(`From: =?ISO-8859-1?Q?Andr=E9?= <andre@example.org>
To: bob@example.org, "Carol, C." <carol@example.org>
Cc: dave@example.org
Subject: =?UTF-8?B?w4ljaGFudGlsbG9u?=
 folded
Date: Tue, 14 Oct 2026 09:30:00 +0200
Message-ID: <abc@example.org>
References: <root@example.org> <mid@example.org>
In-Reply-To: <mid@example.org>

body
`)
	h, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := Header{
		MessageID:  "abc@example.org",
		Date:       time.Date(2026, 10, 14, 7, 30, 0, 0, time.UTC),
		From:       "André <andre@example.org>",
		To:         []string{"bob@example.org", "Carol, C. <carol@example.org>", "dave@example.org"},
		Subject:    "Échantillon folded",
		References: []string{"root@example.org", "mid@example.org"},
		InReplyTo:  "mid@example.org",
	}
	if h.MessageID != want.MessageID || !h.Date.Equal(want.Date) || h.From != want.From ||
		!slices.Equal(h.To, want.To) || h.Subject != want.Subject ||
		!slices.Equal(h.References, want.References) || h.InReplyTo != want.InReplyTo {
		t.Errorf("Parse =\n%+v\nwant\n%+v", h, want)
	}
	if got := ThreadKey(h, "x"); got != "root@example.org" {
		t.Errorf("ThreadKey = %q", got)
	}
}

func TestParseLenient(t *testing.T) {
	raw := crlf(`From: not an address
Subject: plain
Date: yesterday-ish
Message-ID: no-brackets@example.org
References: garbage <ok@example.org>

body
`)
	h, err := Parse(raw)
	if err == nil {
		t.Error("expected an informational error for the bad date")
	}
	if h.From != "not an address" || h.Subject != "plain" || h.MessageID != "no-brackets@example.org" {
		t.Errorf("lenient parse = %+v", h)
	}
	if !h.Date.IsZero() {
		t.Errorf("Date = %v, want zero", h.Date)
	}
	if len(h.References) == 0 {
		t.Error("lost References")
	}
}

func TestParseEmptyAndGarbage(t *testing.T) {
	for _, raw := range []string{"", "\r\n\r\n", "no colon here\r\n\r\nbody"} {
		h, _ := Parse([]byte(raw))
		if h.MessageID != "" || h.Subject != "" {
			t.Errorf("Parse(%q) = %+v", raw, h)
		}
	}
}

func TestThreadKey(t *testing.T) {
	tests := []struct {
		h    Header
		want string
	}{
		{Header{References: []string{"r"}, InReplyTo: "p", MessageID: "m"}, "r"},
		{Header{InReplyTo: "p", MessageID: "m"}, "p"},
		{Header{MessageID: "m"}, "m"},
		{Header{}, "hash:h"},
	}
	for _, tt := range tests {
		if got := ThreadKey(tt.h, "h"); got != tt.want {
			t.Errorf("ThreadKey(%+v) = %q, want %q", tt.h, got, tt.want)
		}
	}
}

func TestNormalizeMessageID(t *testing.T) {
	for in, want := range map[string]string{
		"<a@b>":     "a@b",
		"  <a@b>  ": "a@b",
		"a@b":       "a@b",
		"":          "",
		"<>":        "",
	} {
		if got := NormalizeMessageID(in); got != want {
			t.Errorf("NormalizeMessageID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBodiesMultipart(t *testing.T) {
	raw := crlf(`From: a@example.org
Content-Type: multipart/mixed; boundary=outer

--outer
Content-Type: multipart/alternative; boundary=inner

--inner
Content-Type: text/plain; charset=iso-8859-1
Content-Transfer-Encoding: quoted-printable

caf=E9
--inner
Content-Type: text/html; charset=utf-8

<p>caf&eacute;</p>
--inner--
--outer
Content-Type: application/pdf; name="=?UTF-8?Q?r=C3=A9sum=C3=A9.pdf?="
Content-Disposition: attachment
Content-Transfer-Encoding: base64

aGVsbG8=
--outer
Content-Type: text/plain
Content-Disposition: attachment; filename=notes.txt

attached text
--outer--
`)
	c, err := Bodies(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(c.Plain) != "café" {
		t.Errorf("Plain = %q", c.Plain)
	}
	if !strings.Contains(c.HTML, "<p>caf&eacute;</p>") {
		t.Errorf("HTML = %q", c.HTML)
	}
	want := []Attachment{
		{Filename: "résumé.pdf", ContentType: "application/pdf", Size: 5},
		{Filename: "notes.txt", ContentType: "text/plain", Size: 13},
	}
	if !slices.Equal(c.Attachments, want) {
		t.Errorf("Attachments = %+v, want %+v", c.Attachments, want)
	}
}

func TestBodiesSinglePartAndUnknownCharset(t *testing.T) {
	c, err := Bodies(crlf("Subject: x\n\nhello\n"))
	if err != nil || strings.TrimSpace(c.Plain) != "hello" {
		t.Errorf("default text/plain: %q, %v", c.Plain, err)
	}
	c, err = Bodies(crlf("Content-Type: text/plain; charset=x-unknown\n\nstill readable\n"))
	if err != nil || !strings.Contains(c.Plain, "still readable") {
		t.Errorf("unknown charset: %q, %v", c.Plain, err)
	}
}

func TestBodiesTooManyParts(t *testing.T) {
	var b strings.Builder
	b.WriteString("Content-Type: multipart/mixed; boundary=b\r\n\r\n")
	for range maxParts + 5 {
		b.WriteString("--b\r\nContent-Type: text/plain\r\n\r\nx\r\n")
	}
	b.WriteString("--b--\r\n")
	if _, err := Bodies([]byte(b.String())); err == nil {
		t.Fatal("expected an error for too many parts")
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte("From: a@b\r\nSubject: =?UTF-8?Q?x?=\r\nMessage-ID: <x@y>\r\n\r\nbody"))
	f.Add([]byte("Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n\r\nhi\r\n--x--"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		h, _ := Parse(raw)
		_ = ThreadKey(h, "h")
		_, _ = Bodies(raw)
		_ = NormalizeMessageID(string(raw))
	})
}
